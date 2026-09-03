package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
)

const csrfCookieName = "rotup_csrf"
const csrfFieldName = "csrf_token"

// csrfGuard implements double-submit CSRF protection for the local UI.
//
// v1 binds to loopback and has a single administrator, so there is no session
// or login to protect. The risk this closes is a page in the administrator's
// own browser posting to 127.0.0.1: without a token that page cannot read, it
// could otherwise create sources or start runs.
type csrfGuard struct{}

func newCSRFGuard() *csrfGuard { return &csrfGuard{} }

// token returns the request's token, issuing and setting a new one when the
// browser does not have one yet.
func (g *csrfGuard) token(response http.ResponseWriter, request *http.Request) string {
	if cookie, err := request.Cookie(csrfCookieName); err == nil && validToken(cookie.Value) {
		return cookie.Value
	}
	issued := newToken()
	http.SetCookie(response, &http.Cookie{
		Name:     csrfCookieName,
		Value:    issued,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	return issued
}

// check validates a mutating request. It returns a safe message when the
// request must be refused.
func (g *csrfGuard) check(request *http.Request) string {
	if origin := request.Header.Get("Origin"); origin != "" && origin != "null" {
		parsed, err := url.Parse(origin)
		if err != nil || !strings.EqualFold(parsed.Host, request.Host) {
			return "the request came from another origin and was refused"
		}
	}
	cookie, err := request.Cookie(csrfCookieName)
	if err != nil || !validToken(cookie.Value) {
		return "this page has no valid form token; reload the page and try again"
	}
	submitted := request.PostFormValue(csrfFieldName)
	if subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(submitted)) != 1 {
		return "the form token did not match; reload the page and try again"
	}
	return ""
}

func newToken() string {
	var value [32]byte
	if _, err := rand.Read(value[:]); err != nil {
		// A token that cannot be generated must not silently become a constant.
		// Returning an empty value makes every check fail closed.
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(value[:])
}

func validToken(value string) bool {
	return len(value) == 43
}
