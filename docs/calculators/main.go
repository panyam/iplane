package main

import (
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/felixge/httpsnoop"
	"github.com/gorilla/mux"
	s3 "github.com/panyam/s3gen"
)

var (
	addr  = flag.String("addr", DefaultAddress(), "Address to serve on")
	build = flag.Bool("build", false, "Generate the static site into OutputDir and exit")
)

// BasePath is the URL path the generated site is rooted at. It is empty for
// local development and for any host that serves the calculators at the domain
// root. GitHub project Pages serve at /<repo>/, so a Pages build sets
// CALC_BASE_PATH=/iplane and every template asks Site.PathRelUrl for its links.
// Getting this wrong produces a site whose pages render and whose stylesheets,
// scripts and links all 404, so it is worth checking in the built output.
func BasePath() string {
	return os.Getenv("CALC_BASE_PATH")
}

// BookTitle is the one place the book's title is written on this site. It is
// still provisional: the publisher has "Self-Hosted LLM Inference" on record and
// the author has asked to keep this one as the main title. When that is settled,
// change it here (or set BOOK_TITLE) and republish; nothing else names the book.
func BookTitle() string {
	if t := os.Getenv("BOOK_TITLE"); t != "" {
		return t
	}
	return "Inference Is All You Need"
}

var site = s3.Site{
	PathPrefix: BasePath(),
	CommonFuncMap: map[string]any{
		"BookTitle": BookTitle,
	},
	OutputDir:   "./output",
	ContentRoot: "./content",
	TemplateFolders: []string{
		"./templates",
	},
	StaticFolders: []string{
		"/static/", "static",
	},
	DefaultBaseTemplate: s3.BaseTemplate{
		Name: "CalculatorBase.html",
	},
	AssetPatterns: []string{
		"*.png", "*.jpg", "*.jpeg", "*.gif", "*.svg", "*.webp",
		"*.js", "*.css", "*.json",
	},
	BuildRules: []s3.Rule{},
}

func main() {
	flag.Parse()

	// A one-shot generate, for publishing. Everything the calculators need at
	// runtime is a file server, so the output directory is the whole deployable.
	if *build {
		site.Init()
		site.Rebuild(nil)
		log.Printf("generated %s with base path %q", site.OutputDir, site.PathPrefix)
		return
	}

	if os.Getenv("APP_ENV") != "production" {
		site.Watch()
	}

	router := mux.NewRouter()
	router.PathPrefix(site.PathPrefix).Handler(http.StripPrefix(site.PathPrefix, &site))

	srv := &http.Server{
		Handler: withLogger(router),
		Addr:    *addr,
	}
	log.Printf("'Inference is all you need' Book Calculators serving on %s", *addr)
	log.Fatal(srv.ListenAndServe())
}

func DefaultAddress() string {
	port := os.Getenv("CALC_PORT")
	if port != "" {
		return port
	}
	return ":8088"
}

func withLogger(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		m := httpsnoop.CaptureMetrics(handler, writer, request)
		log.Printf("http[%d] %s %s\n", m.Code, m.Duration, request.URL.Path)
	})
}
