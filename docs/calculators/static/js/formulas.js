/**
 * Inference Book - Formula Definitions
 * Each formula has calculation logic, inputs, and display metadata
 */

const FORMULAS = {
  // Chapter 3, Section 3.1 - Weight Memory Calculator
  vram: {
    id: 'vram',
    title: 'Weight Memory Calculator',
    chapter: 3,
    section: '3.1',
    description: 'Calculate GPU memory required for model weights at different precisions',
    latex: '\\text{Memory (GB)} = \\frac{P \\times B}{10^9}',

    inputs: [
      {
        id: 'p',
        label: 'Parameters',
        unit: 'billion',
        type: 'number',
        default: 7,
        min: 0.1,
        max: 1000,
        step: 0.1,
        hint: 'Model size (e.g., 7 for Llama 7B)'
      },
      {
        id: 'q',
        label: 'Precision',
        type: 'select',
        default: 2,
        options: [
          { value: 4, label: 'FP32 (4 bytes)' },
          { value: 2, label: 'FP16/BF16 (2 bytes)' },
          { value: 1, label: 'INT8 (1 byte)' },
          { value: 0.5, label: 'INT4 (0.5 bytes)' }
        ],
        hint: 'Bytes per parameter'
      }
    ],

    presets: [
      { label: '7B Q4', values: { p: 7, q: 0.5 } },
      { label: '7B FP16', values: { p: 7, q: 2 } },
      { label: '13B Q4', values: { p: 13, q: 0.5 } },
      { label: '30B Q4', values: { p: 30, q: 0.5 } },
      { label: '70B Q4', values: { p: 70, q: 0.5 } },
      { label: '70B FP16', values: { p: 70, q: 2 } }
    ],

    calculate(inputs) {
      const p = inputs.p * 1e9; // Convert billions to actual
      const b = inputs.q;
      const memoryBytes = p * b;
      const memoryGB = memoryBytes / 1e9;

      return {
        result: memoryGB,
        unit: 'GB',
        breakdown: [
          { label: 'Parameters', value: `${inputs.p}B` },
          { label: 'Bytes per param', value: `${inputs.q}` },
          { label: 'Total bytes', value: `${(memoryBytes / 1e9).toFixed(1)} GB` }
        ]
      };
    }
  },

  // Chapter 3, Section 3.1.2 - KV Cache Calculator
  kv: {
    id: 'kv',
    title: 'KV Cache Calculator',
    chapter: 3,
    section: '3.1.2',
    description: 'Calculate memory required for Key-Value cache during inference',
    latex: '\\text{KV Cache} = 2 \\times L \\times H \\times D \\times C \\times B \\times N',

    inputs: [
      {
        id: 'l',
        label: 'Layers',
        type: 'number',
        default: 32,
        min: 1,
        max: 200,
        hint: 'Number of transformer layers'
      },
      {
        id: 'h',
        label: 'KV Heads',
        type: 'number',
        default: 32,
        min: 1,
        max: 128,
        hint: 'Number of key-value heads (may differ from attention heads with GQA)'
      },
      {
        id: 'd',
        label: 'Head Dimension',
        type: 'number',
        default: 128,
        min: 32,
        max: 256,
        hint: 'Dimension per attention head'
      },
      {
        id: 'c',
        label: 'Context Length',
        unit: 'tokens',
        type: 'number',
        default: 4096,
        min: 128,
        max: 1000000,
        step: 128,
        hint: 'Maximum sequence length'
      },
      {
        id: 'b',
        label: 'Bytes per Value',
        type: 'select',
        default: 2,
        options: [
          { value: 4, label: 'FP32 (4 bytes)' },
          { value: 2, label: 'FP16 (2 bytes)' },
          { value: 1, label: 'INT8 (1 byte)' }
        ]
      },
      {
        id: 'n',
        label: 'Batch Size',
        type: 'number',
        default: 1,
        min: 1,
        max: 256,
        hint: 'Number of concurrent sequences'
      }
    ],

    presets: [
      { label: '7B 4K', values: { l: 32, h: 32, d: 128, c: 4096, b: 2, n: 1 } },
      { label: '7B 32K', values: { l: 32, h: 32, d: 128, c: 32768, b: 2, n: 1 } },
      { label: '70B 4K', values: { l: 80, h: 8, d: 128, c: 4096, b: 2, n: 1 } },
      { label: '70B 128K', values: { l: 80, h: 8, d: 128, c: 131072, b: 2, n: 1 } }
    ],

    calculate(inputs) {
      const { l, h, d, c, b, n } = inputs;
      // 2 for keys and values
      const cacheBytes = 2 * l * h * d * c * b * n;
      const cacheGB = cacheBytes / 1e9;

      return {
        result: cacheGB,
        unit: 'GB',
        breakdown: [
          { label: 'Per token', value: `${((2 * l * h * d * b) / 1024).toFixed(1)} KB` },
          { label: 'Per sequence', value: `${((2 * l * h * d * c * b) / 1e6).toFixed(1)} MB` },
          { label: 'Total (batch)', value: `${cacheGB.toFixed(2)} GB` }
        ]
      };
    }
  },

  // Chapter 3, Section 3.2 - Total VRAM Calculator
  total: {
    id: 'total',
    title: 'Total VRAM Calculator',
    chapter: 3,
    section: '3.2',
    description: 'Calculate total GPU memory including weights, KV cache, and overhead',
    latex: '\\text{Total} \\approx (\\text{Weights} + \\text{KV Cache} + \\text{Activations}) \\times 1.15',

    inputs: [
      {
        id: 'p',
        label: 'Parameters',
        unit: 'billion',
        type: 'number',
        default: 7,
        min: 0.1,
        max: 1000,
        step: 0.1
      },
      {
        id: 'q',
        label: 'Quantization',
        type: 'select',
        default: 0.5,
        options: [
          { value: 4, label: 'FP32 (4 bytes)' },
          { value: 2, label: 'FP16/BF16 (2 bytes)' },
          { value: 1, label: 'INT8 (1 byte)' },
          { value: 0.5, label: 'INT4 (0.5 bytes)' }
        ]
      },
      {
        id: 'c',
        label: 'Context Length',
        unit: 'tokens',
        type: 'number',
        default: 4096,
        min: 128,
        max: 1000000,
        step: 128
      },
      {
        id: 'n',
        label: 'Batch Size',
        type: 'number',
        default: 1,
        min: 1,
        max: 256
      },
      {
        id: 'overhead',
        label: 'Overhead',
        type: 'select',
        default: 1.15,
        options: [
          { value: 1.10, label: '10% (aggressive)' },
          { value: 1.15, label: '15% (recommended)' },
          { value: 1.20, label: '20% (conservative)' }
        ]
      }
    ],

    presets: [
      { label: '7B Q4 4K', values: { p: 7, q: 0.5, c: 4096, n: 1, overhead: 1.15 } },
      { label: '7B FP16 4K', values: { p: 7, q: 2, c: 4096, n: 1, overhead: 1.15 } },
      { label: '30B Q4 8K', values: { p: 30, q: 0.5, c: 8192, n: 1, overhead: 1.15 } },
      { label: '70B Q4 8K', values: { p: 70, q: 0.5, c: 8192, n: 1, overhead: 1.15 } }
    ],

    calculate(inputs) {
      const { p, q, c, n, overhead } = inputs;

      // Weights
      const weightsGB = (p * 1e9 * q) / 1e9;

      // Estimate KV cache (simplified: assume typical architecture)
      // Roughly: layers ≈ 2 * sqrt(params_B), heads ≈ sqrt(params_B) * 4
      const layers = Math.round(2 * Math.sqrt(p) + 20);
      const kvHeads = Math.max(8, Math.round(Math.sqrt(p) * 4));
      const headDim = 128;
      const kvBytes = q >= 1 ? 2 : 2; // KV usually FP16 even with quantized weights
      const kvCacheGB = (2 * layers * kvHeads * headDim * c * kvBytes * n) / 1e9;

      // Activations (rough estimate)
      const activationsGB = (n * c * 4096 * layers * 3) / 1e9;

      const subtotal = weightsGB + kvCacheGB + activationsGB;
      const total = subtotal * overhead;

      return {
        result: total,
        unit: 'GB',
        breakdown: [
          { label: 'Weights', value: `${weightsGB.toFixed(1)} GB` },
          { label: 'KV Cache (est.)', value: `${kvCacheGB.toFixed(1)} GB` },
          { label: 'Activations (est.)', value: `${activationsGB.toFixed(2)} GB` },
          { label: 'Subtotal', value: `${subtotal.toFixed(1)} GB` },
          { label: `+ ${((overhead - 1) * 100).toFixed(0)}% overhead`, value: `${(subtotal * (overhead - 1)).toFixed(1)} GB` },
          { label: 'Total Required', value: `${total.toFixed(1)} GB` }
        ]
      };
    }
  },

  // Chapter 3, Section 3.5.3 - Break-Even Calculator
  breakeven: {
    id: 'breakeven',
    title: 'Break-Even Calculator',
    chapter: 3,
    section: '3.5.3',
    description: 'Calculate when buying hardware becomes cheaper than renting',
    latex: '\\text{Break-even (months)} = \\frac{\\text{Hardware} + \\text{Setup}}{\\text{Rental} - \\text{Operating}}',

    inputs: [
      {
        id: 'hw',
        label: 'Hardware Cost',
        unit: '$',
        type: 'number',
        default: 2000,
        min: 100,
        max: 500000,
        step: 100,
        hint: 'One-time purchase price'
      },
      {
        id: 'setup',
        label: 'Setup Costs',
        unit: '$',
        type: 'number',
        default: 0,
        min: 0,
        max: 50000,
        step: 100,
        hint: 'Installation, networking, etc.'
      },
      {
        id: 'rent',
        label: 'Rental Cost',
        unit: '$/hour',
        type: 'number',
        default: 0.50,
        min: 0.01,
        max: 50,
        step: 0.01,
        hint: 'Cloud GPU hourly rate'
      },
      {
        id: 'hours',
        label: 'Usage',
        unit: 'hours/month',
        type: 'number',
        default: 720,
        min: 1,
        max: 744,
        step: 1,
        hint: '720 = 24/7, 160 = business hours'
      },
      {
        id: 'power',
        label: 'Power Cost',
        unit: '$/kWh',
        type: 'number',
        default: 0.12,
        min: 0.01,
        max: 1,
        step: 0.01
      },
      {
        id: 'watts',
        label: 'GPU Power Draw',
        unit: 'watts',
        type: 'number',
        default: 350,
        min: 50,
        max: 1000,
        step: 10
      }
    ],

    presets: [
      { label: 'RTX 4090', values: { hw: 2000, setup: 0, rent: 0.50, hours: 720, power: 0.12, watts: 450 } },
      { label: 'RTX 4070 Ti', values: { hw: 800, setup: 0, rent: 0.30, hours: 720, power: 0.12, watts: 285 } },
      { label: 'A100 80GB', values: { hw: 15000, setup: 500, rent: 2.50, hours: 720, power: 0.12, watts: 400 } },
      { label: 'H100 80GB', values: { hw: 30000, setup: 1000, rent: 4.00, hours: 720, power: 0.15, watts: 700 } }
    ],

    calculate(inputs) {
      const { hw, setup, rent, hours, power, watts } = inputs;

      // Monthly costs
      const monthlyRental = rent * hours;
      const monthlyPower = (watts / 1000) * hours * power;
      const monthlyOperating = monthlyPower; // Simplified, could add cooling, maintenance

      const capitalCost = hw + setup;
      const monthlySavings = monthlyRental - monthlyOperating;

      let breakEvenMonths = Infinity;
      let recommendation = 'Rent';

      if (monthlySavings > 0) {
        breakEvenMonths = capitalCost / monthlySavings;
        if (breakEvenMonths <= 12) {
          recommendation = 'Buy (< 1 year payback)';
        } else if (breakEvenMonths <= 24) {
          recommendation = 'Consider buying (1-2 year payback)';
        } else {
          recommendation = 'Likely rent (> 2 year payback)';
        }
      }

      return {
        result: breakEvenMonths === Infinity ? '∞' : breakEvenMonths.toFixed(1),
        unit: 'months',
        breakdown: [
          { label: 'Monthly rental', value: `$${monthlyRental.toFixed(0)}` },
          { label: 'Monthly power', value: `$${monthlyPower.toFixed(0)}` },
          { label: 'Monthly savings', value: `$${monthlySavings.toFixed(0)}` },
          { label: 'Capital cost', value: `$${capitalCost.toLocaleString()}` },
          { label: 'Recommendation', value: recommendation }
        ]
      };
    }
  },

  // Chapter 2, Section 2.5 - Model Parameter Calculator
  params: {
    id: 'params',
    title: 'Model Parameter Calculator',
    chapter: 2,
    section: '2.5',
    description: 'Estimate total parameters from model architecture dimensions',
    latex: '\\text{Total} \\approx \\text{Embedding} + L \\times (\\text{Attention} + \\text{FFN})',

    inputs: [
      {
        id: 'layers',
        label: 'Layers',
        type: 'number',
        default: 32,
        min: 1,
        max: 200,
        hint: 'Number of transformer layers'
      },
      {
        id: 'hidden',
        label: 'Hidden Dimension',
        type: 'number',
        default: 4096,
        min: 256,
        max: 16384,
        step: 256,
        hint: 'Model hidden size (d_model)'
      },
      {
        id: 'heads',
        label: 'Attention Heads',
        type: 'number',
        default: 32,
        min: 1,
        max: 128,
        hint: 'Number of attention heads'
      },
      {
        id: 'vocab',
        label: 'Vocabulary Size',
        type: 'number',
        default: 32000,
        min: 1000,
        max: 200000,
        step: 1000,
        hint: 'Number of tokens in vocabulary'
      },
      {
        id: 'ffn_mult',
        label: 'FFN Multiplier',
        type: 'select',
        default: 4,
        options: [
          { value: 4, label: '4x (standard)' },
          { value: 2.67, label: '2.67x (SwiGLU effective)' },
          { value: 8, label: '8x (large FFN)' }
        ],
        hint: 'FFN intermediate size / hidden size'
      },
      {
        id: 'ffn_type',
        label: 'FFN Type',
        type: 'select',
        default: 'swiglu',
        options: [
          { value: 'standard', label: 'Standard (2 matrices)' },
          { value: 'swiglu', label: 'SwiGLU (3 matrices)' }
        ],
        hint: 'FFN architecture type'
      }
    ],

    presets: [
      { label: 'Llama 7B', values: { layers: 32, hidden: 4096, heads: 32, vocab: 32000, ffn_mult: 2.67, ffn_type: 'swiglu' } },
      { label: 'Llama 13B', values: { layers: 40, hidden: 5120, heads: 40, vocab: 32000, ffn_mult: 2.67, ffn_type: 'swiglu' } },
      { label: 'Llama 70B', values: { layers: 80, hidden: 8192, heads: 64, vocab: 32000, ffn_mult: 2.67, ffn_type: 'swiglu' } },
      { label: 'GPT-2 Small', values: { layers: 12, hidden: 768, heads: 12, vocab: 50257, ffn_mult: 4, ffn_type: 'standard' } }
    ],

    calculate(inputs) {
      const { layers, hidden, heads, vocab, ffn_mult, ffn_type } = inputs;
      const headDim = hidden / heads;

      // Embedding parameters
      const embedding = vocab * hidden;

      // Attention parameters per layer: Q, K, V, O projections
      // Each is hidden x hidden (or hidden x (heads * head_dim))
      const attnPerLayer = 4 * hidden * hidden;

      // FFN parameters per layer
      let ffnPerLayer;
      const ffnIntermediate = hidden * ffn_mult;
      if (ffn_type === 'swiglu') {
        // SwiGLU has 3 matrices: up, gate, down
        ffnPerLayer = 3 * hidden * ffnIntermediate;
      } else {
        // Standard FFN has 2 matrices: up, down
        ffnPerLayer = 2 * hidden * ffnIntermediate;
      }

      // Layer norm parameters (small but included for accuracy)
      const layerNormPerLayer = 4 * hidden; // 2 layer norms with weight + bias each

      // Total per layer
      const perLayer = attnPerLayer + ffnPerLayer + layerNormPerLayer;

      // Output projection (often tied with embedding)
      const output = vocab * hidden;

      // Total
      const total = embedding + (layers * perLayer) + output;
      const totalB = total / 1e9;

      return {
        result: totalB,
        unit: 'B params',
        breakdown: [
          { label: 'Embedding', value: `${(embedding / 1e6).toFixed(0)}M` },
          { label: 'Attention/layer', value: `${(attnPerLayer / 1e6).toFixed(0)}M` },
          { label: 'FFN/layer', value: `${(ffnPerLayer / 1e6).toFixed(0)}M` },
          { label: 'Per layer total', value: `${(perLayer / 1e6).toFixed(0)}M` },
          { label: `${layers} layers`, value: `${((layers * perLayer) / 1e9).toFixed(2)}B` },
          { label: 'Output projection', value: `${(output / 1e6).toFixed(0)}M` },
          { label: 'Total', value: `${totalB.toFixed(2)}B` }
        ]
      };
    }
  },

  // Chapter 2, Section 2.4.3 - FFN Parameters Calculator
  ffn: {
    id: 'ffn',
    title: 'FFN Parameters Calculator',
    chapter: 2,
    section: '2.4.3',
    description: 'Calculate feed-forward network parameters per layer',
    latex: '\\text{FFN params} = k \\times d \\times d_{ff} \\text{ where } k=2 \\text{ (standard) or } 3 \\text{ (SwiGLU)}',

    inputs: [
      {
        id: 'hidden',
        label: 'Hidden Dimension',
        unit: 'd',
        type: 'number',
        default: 4096,
        min: 256,
        max: 16384,
        step: 256,
        hint: 'Model hidden size'
      },
      {
        id: 'expansion',
        label: 'Expansion Factor',
        type: 'select',
        default: 4,
        options: [
          { value: 4, label: '4x (standard)' },
          { value: 2.67, label: '2.67x (Llama-style)' },
          { value: 8, label: '8x (large)' }
        ],
        hint: 'FFN intermediate / hidden'
      },
      {
        id: 'ffn_type',
        label: 'FFN Type',
        type: 'select',
        default: 'swiglu',
        options: [
          { value: 'standard', label: 'Standard (2 matrices)' },
          { value: 'swiglu', label: 'SwiGLU (3 matrices)' }
        ]
      },
      {
        id: 'layers',
        label: 'Number of Layers',
        type: 'number',
        default: 32,
        min: 1,
        max: 200,
        hint: 'To calculate total FFN params'
      }
    ],

    presets: [
      { label: '7B Style', values: { hidden: 4096, expansion: 2.67, ffn_type: 'swiglu', layers: 32 } },
      { label: '13B Style', values: { hidden: 5120, expansion: 2.67, ffn_type: 'swiglu', layers: 40 } },
      { label: '70B Style', values: { hidden: 8192, expansion: 2.67, ffn_type: 'swiglu', layers: 80 } },
      { label: 'GPT-2 Style', values: { hidden: 768, expansion: 4, ffn_type: 'standard', layers: 12 } }
    ],

    calculate(inputs) {
      const { hidden, expansion, ffn_type, layers } = inputs;

      const intermediate = Math.round(hidden * expansion);
      const matrices = ffn_type === 'swiglu' ? 3 : 2;
      const paramsPerLayer = matrices * hidden * intermediate;
      const totalParams = paramsPerLayer * layers;

      // Classic formula representation
      const dSquared = hidden * hidden;
      const effectiveMult = (paramsPerLayer / dSquared);

      return {
        result: (paramsPerLayer / 1e6),
        unit: 'M/layer',
        breakdown: [
          { label: 'Hidden dim (d)', value: hidden.toLocaleString() },
          { label: 'Intermediate dim', value: intermediate.toLocaleString() },
          { label: 'Matrices', value: `${matrices} (${ffn_type})` },
          { label: 'Per layer', value: `${(paramsPerLayer / 1e6).toFixed(0)}M` },
          { label: `As d² multiple`, value: `${effectiveMult.toFixed(1)}d²` },
          { label: `Total (${layers} layers)`, value: `${(totalParams / 1e9).toFixed(2)}B` }
        ]
      };
    }
  }
};

// Export for use in calculator.js
if (typeof module !== 'undefined' && module.exports) {
  module.exports = FORMULAS;
}
