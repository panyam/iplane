# Web Calculators - Architecture

## Overview

Data-driven interactive calculators for the book's formulas. A Go-based static site generator (`s3gen`) serves content pages that are rendered entirely by JavaScript from formula definitions.

## Stack

- **s3gen** (Go) - Static site generator and dev server (port 8088)
- **KaTeX** - LaTeX math rendering
- **Vanilla JS** - No framework dependencies
- **CSS custom properties** - Theming via `:root` variables

## Key Files

| File | Purpose |
|------|---------|
| `main.go` | Server config, routing, live reload |
| `static/js/formulas.js` | All formula definitions (inputs, presets, calculation logic) |
| `static/js/calculator.js` | `Calculator` class - UI rendering, event handling, URL sync |
| `static/css/calculator.css` | All styling including responsive layout |
| `templates/CalculatorBase.html` | Shared page shell template |
| `content/c/{ch}/{id}.html` | Per-calculator frontmatter (title, chapter, formula_id) |

## Data Flow

1. Content file declares `formula_id` in frontmatter
2. Template renders page shell with `<div id="calculator" data-formula="id">`
3. `calculator.js` reads the data attribute, looks up formula in `FORMULAS` object
4. Calculator class renders all UI dynamically from the formula definition
5. Input changes trigger recalculation and URL parameter sync
6. URL parameters allow sharing pre-filled calculator states

## Layout Architecture

The calculator pages use a responsive CSS grid layout:

- **Desktop (>900px)**: Two-column grid - Inputs card left, Results card right. Input fields within the Inputs card also use a 2-column grid.
- **Tablet (600-900px)**: Inputs/Results stack vertically; input fields remain 2 columns.
- **Mobile (<600px)**: Everything single column.

Key CSS classes:
- `.calculator-layout` - Grid container for Inputs + Results cards (side by side on desktop)
- `.inputs-grid` - Grid container for input fields within the Inputs card (2-column on non-mobile)

## Adding a New Calculator

1. Define the formula in `static/js/formulas.js` (inputs, presets, calculate function)
2. Create a content file in `content/c/{chapter}/{id}.html` with frontmatter pointing to the formula_id
3. The template and JS handle everything else automatically
