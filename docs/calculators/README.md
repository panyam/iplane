# Interactive Calculators

The seven calculators that Chapters 2 and 3 of *Inference Is All You Need* link to
by QR code. Live at <https://panyam.github.io/iplane/>.

They do their arithmetic in the browser and need nothing behind them but a file
server. KaTeX is vendored in `static/vendor/katex/` rather than loaded from a CDN,
because the printed codes outlive anyone else's hosting.

## Features

- **KaTeX formula rendering** - Beautiful math display
- **URL query parameters** - Pre-populate values from book examples
- **Quick presets** - Common configurations for different model sizes
- **Copy link** - Share calculator state with others
- **Responsive two-column layout** - Inputs left / Results right on desktop; single column on mobile

## Calculators

### Chapter 2: How LLMs Work

| Calculator | URL | Description |
|------------|-----|-------------|
| Model Parameters | `/c/2/params/` | Estimate total parameters from architecture |
| FFN Parameters | `/c/2/ffn/` | Calculate FFN parameters per layer |

### Chapter 3: Hardware Fundamentals

| Calculator | URL | Description |
|------------|-----|-------------|
| Weight Memory | `/c/3/vram/` | Calculate GPU memory for model weights |
| KV Cache | `/c/3/kv/` | Calculate KV cache memory requirements |
| Total VRAM | `/c/3/total/` | Estimate total GPU memory needed |
| Break-Even | `/c/3/breakeven/` | Buy vs rent analysis |

## Running Locally

```bash
cd web
go mod tidy
go run main.go
# Open http://localhost:8088
```

## URL Structure

```
/                    # Calculator index
/c/3/                # Chapter 3 calculators
/c/3/vram/           # Weight Memory calculator
/c/3/vram/?p=70&q=0.5  # With preset values (70B, INT4)
```

## Query Parameters

### Model Parameters (`/c/2/params/`)
- `layers` - Number of transformer layers
- `hidden` - Hidden dimension (d_model)
- `heads` - Number of attention heads
- `vocab` - Vocabulary size
- `ffn_mult` - FFN expansion multiplier (4, 2.67, 8)
- `ffn_type` - FFN type (standard, swiglu)

### FFN Parameters (`/c/2/ffn/`)
- `hidden` - Hidden dimension
- `expansion` - Expansion factor (4, 2.67, 8)
- `ffn_type` - FFN type (standard, swiglu)
- `layers` - Number of layers

### Weight Memory (`/c/3/vram/`)
- `p` - Parameters in billions (default: 7)
- `q` - Bytes per parameter (4=FP32, 2=FP16, 1=INT8, 0.5=INT4)

### KV Cache (`/c/3/kv/`)
- `l` - Layers
- `h` - KV heads
- `d` - Head dimension
- `c` - Context length
- `b` - Bytes per value
- `n` - Batch size

### Total VRAM (`/c/3/total/`)
- `p` - Parameters (billions)
- `q` - Quantization (bytes)
- `c` - Context length
- `n` - Batch size
- `overhead` - Overhead multiplier (1.10, 1.15, 1.20)

### Break-Even (`/c/3/breakeven/`)
- `hw` - Hardware cost ($)
- `setup` - Setup costs ($)
- `rent` - Rental rate ($/hour)
- `hours` - Usage hours/month
- `power` - Power cost ($/kWh)
- `watts` - GPU wattage

## Tech Stack

- **s3gen** - Static site generator (Go)
- **KaTeX** - Math rendering
- **Vanilla JS** - No framework dependencies

## Book Integration

Add calculator links to LaTeX chapters:

```latex
\calclink{/c/3/vram?p=7&q=0.5}{vram-7b-q4}
```

## Publishing

```bash
make export-pages          # builds ./output with CALC_BASE_PATH=/iplane
```

`output/` is the whole deployable. Publishing is that tree pushed to the
`gh-pages` branch of `panyam/iplane`, which is what GitHub Pages serves.

`CALC_BASE_PATH` must match the path the site is served at. GitHub project Pages
serve at `/<repo>/`, so a Pages build needs `/iplane`; a custom domain serving at a
root needs it empty. Every link goes through `Site.PathRelUrl`, so setting it is
enough. Get it wrong and each page still renders while its styles, scripts and
links all 404, which is not visible in the markup. Check by fetching the URLs.

The book prints these links from `\calcbaseurl` in `src/styles/shared-preamble.tex`
in the book repo. That string and `CALC_BASE_PATH` have to change together.
