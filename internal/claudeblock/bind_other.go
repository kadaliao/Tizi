//go:build !darwin

package claudeblock

func bindToInterface(int, int) error {
	return errUnsupported
}
