// Package redact hides credentials in text that Tack prints or records.
// Every message that comes from outside Tack, such as from Flux, Strait,
// git or an API server, passes through it before it reaches the ledger or
// the output ([RFC#0009] Requirement 10).
package redact

import "regexp"

var userinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/\s]+@`)

// Text replaces the user and password of every URL in s with ***.
func Text(s string) string { return userinfo.ReplaceAllString(s, "${1}***@") }
