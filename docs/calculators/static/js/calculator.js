/**
 * Inference Book - Calculator UI Logic
 * Handles rendering, input handling, URL sync, and calculations
 */

class Calculator {
  constructor(formulaId, containerId = 'calculator') {
    this.formula = FORMULAS[formulaId];
    if (!this.formula) {
      console.error(`Formula "${formulaId}" not found`);
      return;
    }

    this.container = document.getElementById(containerId);
    this.values = {};

    // Initialize with defaults
    this.formula.inputs.forEach(input => {
      this.values[input.id] = input.default;
    });

    // Override with URL params
    this.loadFromURL();

    // Render and calculate
    this.render();
    this.calculate();

    // Update URL on changes (debounced)
    this.urlUpdateTimeout = null;
  }

  loadFromURL() {
    const params = new URLSearchParams(window.location.search);
    this.formula.inputs.forEach(input => {
      const value = params.get(input.id);
      if (value !== null) {
        this.values[input.id] = parseFloat(value);
      }
    });
  }

  updateURL() {
    clearTimeout(this.urlUpdateTimeout);
    this.urlUpdateTimeout = setTimeout(() => {
      const params = new URLSearchParams();
      this.formula.inputs.forEach(input => {
        if (this.values[input.id] !== input.default) {
          params.set(input.id, this.values[input.id]);
        }
      });

      const newURL = params.toString()
        ? `${window.location.pathname}?${params.toString()}`
        : window.location.pathname;

      window.history.replaceState({}, '', newURL);

      // Update share URL display
      const urlDisplay = document.getElementById('share-url-display');
      if (urlDisplay) {
        urlDisplay.textContent = window.location.href;
      }
    }, 300);
  }

  render() {
    // Formula display (KaTeX)
    const formulaHTML = `
      <div class="formula-box" id="formula-display"></div>
    `;

    // Presets
    let presetsHTML = '';
    if (this.formula.presets && this.formula.presets.length > 0) {
      presetsHTML = `
        <div class="presets">
          <label>Quick presets:</label>
          ${this.formula.presets.map((preset, i) => `
            <button class="preset-btn" data-preset="${i}">${preset.label}</button>
          `).join('')}
        </div>
      `;
    }

    // Inputs
    const inputsHTML = this.formula.inputs.map(input => this.renderInput(input)).join('');

    // Result
    const resultHTML = `
      <div class="result-box" id="result-box">
        <div class="label">Result</div>
        <div class="value" id="result-value">-</div>
      </div>
      <div class="breakdown" id="breakdown"></div>
    `;

    // Share URL
    const shareHTML = `
      <div class="share-url">
        <button id="copy-url-btn">Copy Link with Values</button>
        <div class="url-display" id="share-url-display">${window.location.href}</div>
      </div>
    `;

    this.container.innerHTML = `
      ${formulaHTML}
      <div class="calculator-layout">
        <div class="card">
          <h2>Inputs</h2>
          ${presetsHTML}
          <div class="inputs-grid">
            ${inputsHTML}
          </div>
        </div>
        <div class="card">
          <h2>Result</h2>
          ${resultHTML}
          ${shareHTML}
        </div>
      </div>
    `;

    // Render formula with KaTeX
    if (window.katex && this.formula.latex) {
      katex.render(this.formula.latex, document.getElementById('formula-display'), {
        throwOnError: false,
        displayMode: true
      });
    }

    // Attach event listeners
    this.attachListeners();
  }

  renderInput(input) {
    const value = this.values[input.id];

    if (input.type === 'select') {
      const options = input.options.map(opt =>
        `<option value="${opt.value}" ${opt.value === value ? 'selected' : ''}>${opt.label}</option>`
      ).join('');

      return `
        <div class="input-group">
          <label for="${input.id}">${input.label}</label>
          <select id="${input.id}" data-input="${input.id}">
            ${options}
          </select>
          ${input.hint ? `<div class="hint">${input.hint}</div>` : ''}
        </div>
      `;
    }

    // Number input
    const unit = input.unit ? ` (${input.unit})` : '';
    return `
      <div class="input-group">
        <label for="${input.id}">${input.label}${unit}</label>
        <input type="number"
               id="${input.id}"
               data-input="${input.id}"
               value="${value}"
               min="${input.min || 0}"
               max="${input.max || 1000000}"
               step="${input.step || 1}">
        ${input.hint ? `<div class="hint">${input.hint}</div>` : ''}
      </div>
    `;
  }

  attachListeners() {
    // Input changes
    this.container.querySelectorAll('[data-input]').forEach(el => {
      el.addEventListener('input', (e) => {
        const inputId = e.target.dataset.input;
        this.values[inputId] = parseFloat(e.target.value);
        this.calculate();
        this.updateURL();
        this.clearActivePreset();
      });
    });

    // Presets
    this.container.querySelectorAll('.preset-btn').forEach(btn => {
      btn.addEventListener('click', (e) => {
        const presetIndex = parseInt(e.target.dataset.preset);
        this.applyPreset(presetIndex);

        // Update active state
        this.container.querySelectorAll('.preset-btn').forEach(b => b.classList.remove('active'));
        e.target.classList.add('active');
      });
    });

    // Copy URL
    const copyBtn = document.getElementById('copy-url-btn');
    if (copyBtn) {
      copyBtn.addEventListener('click', () => {
        navigator.clipboard.writeText(window.location.href).then(() => {
          copyBtn.textContent = 'Copied!';
          setTimeout(() => {
            copyBtn.textContent = 'Copy Link with Values';
          }, 2000);
        });
      });
    }
  }

  clearActivePreset() {
    this.container.querySelectorAll('.preset-btn').forEach(b => b.classList.remove('active'));
  }

  applyPreset(index) {
    const preset = this.formula.presets[index];
    if (!preset) return;

    Object.entries(preset.values).forEach(([key, value]) => {
      this.values[key] = value;

      // Update input element
      const input = document.getElementById(key);
      if (input) {
        input.value = value;
      }
    });

    this.calculate();
    this.updateURL();
  }

  calculate() {
    const result = this.formula.calculate(this.values);

    // Update result display
    const resultValue = document.getElementById('result-value');
    if (resultValue) {
      const displayValue = typeof result.result === 'number'
        ? result.result.toFixed(result.result >= 100 ? 0 : result.result >= 10 ? 1 : 2)
        : result.result;

      resultValue.innerHTML = `${displayValue}<span class="unit">${result.unit}</span>`;
    }

    // Update breakdown
    const breakdown = document.getElementById('breakdown');
    if (breakdown && result.breakdown) {
      breakdown.innerHTML = result.breakdown.map(row => `
        <div class="breakdown-row">
          <span>${row.label}</span>
          <span>${row.value}</span>
        </div>
      `).join('');
    }
  }
}

// Initialize calculator when DOM is ready
document.addEventListener('DOMContentLoaded', () => {
  // Get formula ID from data attribute on container
  const container = document.getElementById('calculator');
  if (container) {
    const formulaId = container.dataset.formula;
    if (formulaId) {
      window.calc = new Calculator(formulaId);
    }
  }
});
