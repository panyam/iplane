
# CALC_BASE_PATH is the URL path the site is served at. Empty serves at a domain
# root; GitHub project Pages serve at /<repo>/, so a Pages build needs /iplane.
# It has to match the host, or every page renders with dead styles and links.
CALC_BASE_PATH ?=

run:
	go run .

# Generate the static site into ./output. That directory is the whole
# deployable: the calculators do their arithmetic in the browser and need
# nothing behind them but a file server.
export:
	rm -rf output
	CALC_BASE_PATH=$(CALC_BASE_PATH) APP_ENV=production go run . -build
	cp -R static output/static
	test -f output/index.html && test -f output/static/js/formulas.js

# What gets published to GitHub Pages at panyam.github.io/iplane/.
export-pages:
	$(MAKE) export CALC_BASE_PATH=/iplane

.PHONY: run export export-pages
