// Package branding defines the instance-level display identity. Internal
// protocol names remain stable even when a deployment uses a custom brand.
package branding

import "strings"

const DefaultName = "nbco"

// Name returns a normalized display name with the backwards-compatible
// product default.
func Name(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return DefaultName
}
