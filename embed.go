package main

import (
	"embed"
)

//go:embed static/admin-login.html static/admin.html
var staticFS embed.FS

var (
	loginPageHTML []byte
	adminPageHTML []byte
)

func init() {
	var err error
	loginPageHTML, err = staticFS.ReadFile("static/admin-login.html")
	if err != nil {
		panic("failed to read embedded admin-login.html: " + err.Error())
	}
	adminPageHTML, err = staticFS.ReadFile("static/admin.html")
	if err != nil {
		panic("failed to read embedded admin.html: " + err.Error())
	}
}
