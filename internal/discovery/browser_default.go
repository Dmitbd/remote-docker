//go:build !darwin

package discovery

// NewBrowser creates the platform production discovery browser.
func NewBrowser() (Browser, error) {
	return NewZeroconfBrowser()
}
