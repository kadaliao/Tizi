package configshare

import (
	"fmt"
	"strings"

	"rsc.io/qr"
)

const qrQuietZone = 4

type qrBlock func(topBlack, bottomBlack bool) string

// RenderQRCode uses one terminal row for two QR modules. Logical white modules
// are drawn explicitly on dark terminals and left blank on light terminals, so
// the mandatory quiet zone remains high-contrast in either palette.
func RenderQRCode(content string, darkBackground bool) (string, int, error) {
	return renderQRCode(content, func(topBlack, bottomBlack bool) string {
		return paletteHalfBlock(topBlack, bottomBlack, darkBackground)
	})
}

// RenderANSIQRCode uses explicit black and white terminal colors, making CLI
// output independent of the user's terminal background.
func RenderANSIQRCode(content string) (string, int, error) {
	return renderQRCode(content, ansiHalfBlock)
}

func renderQRCode(content string, block qrBlock) (string, int, error) {
	code, err := qr.Encode(content, qr.L)
	if err != nil {
		return "", 0, fmt.Errorf("生成二维码: %w", err)
	}
	width := code.Size + qrQuietZone*2
	var output strings.Builder
	for y := -qrQuietZone; y < code.Size+qrQuietZone; y += 2 {
		for x := -qrQuietZone; x < code.Size+qrQuietZone; x++ {
			output.WriteString(block(code.Black(x, y), code.Black(x, y+1)))
		}
		if y+2 < code.Size+qrQuietZone {
			output.WriteByte('\n')
		}
	}
	return output.String(), width, nil
}

func paletteHalfBlock(topBlack, bottomBlack, darkBackground bool) string {
	if darkBackground {
		switch {
		case topBlack && bottomBlack:
			return " "
		case topBlack:
			return "▄"
		case bottomBlack:
			return "▀"
		default:
			return "█"
		}
	}
	switch {
	case topBlack && bottomBlack:
		return "█"
	case topBlack:
		return "▀"
	case bottomBlack:
		return "▄"
	default:
		return " "
	}
}

func ansiHalfBlock(topBlack, bottomBlack bool) string {
	const reset = "\x1b[0m"
	switch {
	case topBlack && bottomBlack:
		return "\x1b[40m " + reset
	case topBlack:
		return "\x1b[30;47m▀" + reset
	case bottomBlack:
		return "\x1b[97;40m▀" + reset
	default:
		return "\x1b[47m " + reset
	}
}
