import Foundation

struct EngineError: LocalizedError, Decodable {
    var code: String?
    var message: String

    var errorDescription: String? { message }

    static func local(_ message: String) -> EngineError {
        EngineError(code: nil, message: message)
    }
}

/// 没有返回值的方法用它解码 `{}`。
struct Empty: Decodable {}

private struct Envelope: Decodable {
    var id: Int64?
    var event: String?
    var error: EngineError?
}

private struct ResultEnvelope<Result: Decodable>: Decodable {
    var result: Result
}

private struct DataEnvelope<Payload: Decodable>: Decodable {
    var data: Payload
}

/// 以 JSON 行协议驱动 `tizi _gui-engine` 子进程。所有进程和缓冲区状态
/// 只在 `queue` 上访问；事件和退出回调派发到主线程。
final class EngineClient: @unchecked Sendable {
    var onEvent: ((_ name: String, _ line: Data) -> Void)?
    var onExit: ((_ status: Int32, _ stderr: String) -> Void)?

    private let queue = DispatchQueue(label: "tizi.engine")
    private var process: Process?
    private var input: FileHandle?
    private var buffer = Data()
    private var nextID: Int64 = 0
    private var pending: [Int64: (Result<Data, Error>) -> Void] = [:]
    private var stderrLines: [String] = []
    private var stderrPartial = Data()

    func launch(executable: URL, arguments: [String]) throws {
        // 旧进程（例如没有响应的引擎）先断开：关闭它的输入让它自行退出，
        // 并让等待它回应的调用立即失败，而不是永远挂起。
        queue.sync {
            try? input?.close()
            input = nil
            self.process = nil
            failPending("后台引擎已重新启动")
        }
        let process = Process()
        process.executableURL = executable
        process.arguments = arguments
        let stdin = Pipe()
        let stdout = Pipe()
        let stderr = Pipe()
        process.standardInput = stdin
        process.standardOutput = stdout
        process.standardError = stderr
        // 只处理当前进程的输出：重启后旧进程残留的输出不能混进新缓冲区。
        stdout.fileHandleForReading.readabilityHandler = { [weak self, weak process] handle in
            let data = handle.availableData
            if data.isEmpty { handle.readabilityHandler = nil }
            self?.queue.async {
                guard let self, let process, self.process === process else { return }
                self.consume(data)
            }
        }
        stderr.fileHandleForReading.readabilityHandler = { [weak self, weak process] handle in
            let data = handle.availableData
            if data.isEmpty { handle.readabilityHandler = nil }
            self?.queue.async {
                guard let self, let process, self.process === process else { return }
                self.consumeStderr(data)
            }
        }
        process.terminationHandler = { [weak self] finished in
            // 留一点时间让管道里剩余的输出先被处理。
            self?.queue.asyncAfter(deadline: .now() + 0.2) { self?.terminated(finished) }
        }
        try process.run()
        queue.sync {
            self.process = process
            self.input = stdin.fileHandleForWriting
            self.buffer = Data()
            self.stderrLines = []
            self.stderrPartial = Data()
        }
    }

    var isRunning: Bool {
        queue.sync { process?.isRunning ?? false }
    }

    /// 关闭引擎的标准输入。引擎会完成进行中的操作后自行退出，不会停止 Mihomo。
    func shutdown() {
        queue.sync {
            try? input?.close()
            input = nil
        }
    }

    func call<Result: Decodable>(_ method: String, _ params: [String: Any]? = nil, as type: Result.Type = Result.self) async throws -> Result {
        let line: Data = try await withCheckedThrowingContinuation { continuation in
            queue.async {
                guard let input = self.input else {
                    continuation.resume(throwing: EngineError.local("后台引擎未运行"))
                    return
                }
                self.nextID += 1
                let id = self.nextID
                var request: [String: Any] = ["id": id, "method": method]
                if let params { request["params"] = params }
                guard var data = try? JSONSerialization.data(withJSONObject: request) else {
                    continuation.resume(throwing: EngineError.local("无法编码请求 \(method)"))
                    return
                }
                data.append(0x0A)
                self.pending[id] = { continuation.resume(with: $0) }
                do {
                    try input.write(contentsOf: data)
                } catch {
                    self.pending.removeValue(forKey: id)
                    continuation.resume(throwing: EngineError.local("无法与后台引擎通信：\(error.localizedDescription)"))
                }
            }
        }
        if Result.self == Empty.self, let empty = Empty() as? Result {
            return empty
        }
        return try JSONDecoder().decode(ResultEnvelope<Result>.self, from: line).result
    }

    static func decodeEvent<Payload: Decodable>(_ type: Payload.Type, from line: Data) -> Payload? {
        try? JSONDecoder().decode(DataEnvelope<Payload>.self, from: line).data
    }

    private func failPending(_ message: String) {
        let waiting = pending
        pending = [:]
        for completion in waiting.values {
            completion(.failure(EngineError.local(message)))
        }
    }

    private func consume(_ data: Data) {
        buffer.append(data)
        while let newline = buffer.firstIndex(of: 0x0A) {
            let line = buffer.subdata(in: buffer.startIndex..<newline)
            buffer.removeSubrange(buffer.startIndex...newline)
            handle(line)
        }
    }

    private func handle(_ line: Data) {
        guard !line.isEmpty, let envelope = try? JSONDecoder().decode(Envelope.self, from: line) else { return }
        if let event = envelope.event {
            let callback = onEvent
            DispatchQueue.main.async { callback?(event, line) }
            return
        }
        guard let id = envelope.id, let completion = pending.removeValue(forKey: id) else { return }
        if let error = envelope.error {
            completion(.failure(error))
        } else {
            completion(.success(line))
        }
    }

    private func consumeStderr(_ data: Data) {
        stderrPartial.append(data)
        while let newline = stderrPartial.firstIndex(of: 0x0A) {
            let line = String(decoding: stderrPartial.subdata(in: stderrPartial.startIndex..<newline), as: UTF8.self)
            stderrPartial.removeSubrange(stderrPartial.startIndex...newline)
            stderrLines.append(line)
        }
        if data.isEmpty, !stderrPartial.isEmpty {
            stderrLines.append(String(decoding: stderrPartial, as: UTF8.self))
            stderrPartial = Data()
        }
        if stderrLines.count > 200 {
            stderrLines.removeFirst(stderrLines.count - 200)
        }
    }

    private func terminated(_ finished: Process) {
        guard process === finished else { return }
        let status = finished.terminationStatus
        let tail = stderrLines.suffix(20).joined(separator: "\n")
        process = nil
        input = nil
        buffer = Data()
        failPending("后台引擎已退出")
        let callback = onExit
        DispatchQueue.main.async { callback?(status, tail) }
    }
}
