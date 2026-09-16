package main

import (
	"embed"
)

//go:embed static/admin-login.html
var staticFS embed.FS

// loginPageHTML contains the embedded login page content.
var loginPageHTML []byte

func init() {
	data, err := staticFS.ReadFile("static/admin-login.html")
	if err != nil {
		panic("failed to read embedded admin-login.html: " + err.Error())
	}
	loginPageHTML = data
}
