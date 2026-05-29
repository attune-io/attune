# Savings Calculator

> **Note:** This page is an interactive calculator that requires JavaScript.
> If you are viewing this on GitHub, visit the
> [live version](https://attune-io.github.io/attune/savings-calculator/) instead.

Estimate how much you could save by right-sizing your Kubernetes workloads
with attune. Enter your current resource allocation and actual usage
below, and see the projected monthly and annual savings instantly.

The calculator uses the same pricing model as the operator's built-in
`EstimatedMonthlySavings` computation (configurable via `AttuneDefaults`).

---

<style>
.calc-container {
  max-width: 900px;
  margin: 0 auto;
  font-family: inherit;
}
.calc-section {
  background: var(--md-code-bg-color, #f5f5f5);
  border-radius: 8px;
  padding: 24px;
  margin-bottom: 24px;
}
.calc-section h3 {
  margin-top: 0;
  margin-bottom: 16px;
  border-bottom: 2px solid var(--md-primary-fg-color, #009688);
  padding-bottom: 8px;
}
.calc-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 16px;
}
@media (max-width: 600px) {
  .calc-grid { grid-template-columns: 1fr; }
}
.calc-field {
  display: flex;
  flex-direction: column;
}
.calc-field label {
  font-size: 0.85em;
  font-weight: 600;
  margin-bottom: 4px;
  color: var(--md-default-fg-color--light, #666);
}
.calc-field input, .calc-field select {
  padding: 10px 12px;
  border: 1px solid var(--md-default-fg-color--lightest, #ccc);
  border-radius: 6px;
  font-size: 1em;
  background: var(--md-default-bg-color, #fff);
  color: var(--md-default-fg-color, #333);
}
.calc-field input:focus, .calc-field select:focus {
  outline: none;
  border-color: var(--md-primary-fg-color, #009688);
  box-shadow: 0 0 0 2px rgba(0, 150, 136, 0.2);
}
.calc-field .help-text {
  font-size: 0.75em;
  color: var(--md-default-fg-color--lighter, #999);
  margin-top: 2px;
}
.calc-results {
  background: var(--md-primary-fg-color, #009688);
  color: #fff;
  border-radius: 8px;
  padding: 24px;
  margin-top: 24px;
}
.calc-results h3 {
  margin-top: 0;
  color: #fff;
  border-bottom: 2px solid rgba(255,255,255,0.3);
  padding-bottom: 8px;
}
.results-grid {
  display: grid;
  grid-template-columns: 1fr 1fr 1fr;
  gap: 16px;
  margin-bottom: 16px;
}
@media (max-width: 600px) {
  .results-grid { grid-template-columns: 1fr; }
}
.result-card {
  background: rgba(255,255,255,0.15);
  border-radius: 6px;
  padding: 16px;
  text-align: center;
}
.result-card .result-value {
  font-size: 2em;
  font-weight: 700;
  margin: 4px 0;
}
.result-card .result-label {
  font-size: 0.85em;
  opacity: 0.9;
}
.breakdown-table {
  width: 100%;
  border-collapse: collapse;
  margin-top: 16px;
}
.breakdown-table th, .breakdown-table td {
  padding: 8px 12px;
  text-align: left;
  border-bottom: 1px solid rgba(255,255,255,0.2);
  color: #fff;
}
.breakdown-table th {
  font-size: 0.85em;
  text-transform: uppercase;
  opacity: 0.8;
}
.savings-bar-container {
  margin-top: 16px;
}
.savings-bar-label {
  display: flex;
  justify-content: space-between;
  font-size: 0.85em;
  margin-bottom: 4px;
}
.savings-bar {
  height: 24px;
  background: rgba(255,255,255,0.2);
  border-radius: 12px;
  overflow: hidden;
  position: relative;
}
.savings-bar-fill {
  height: 100%;
  border-radius: 12px;
  transition: width 0.4s ease;
}
.savings-bar-fill.cpu { background: #4dd0e1; }
.savings-bar-fill.mem { background: #81c784; }
.calc-presets {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-bottom: 16px;
}
.preset-btn {
  padding: 6px 14px;
  border: 1px solid var(--md-primary-fg-color, #009688);
  border-radius: 20px;
  background: transparent;
  color: var(--md-primary-fg-color, #009688);
  cursor: pointer;
  font-size: 0.85em;
  transition: all 0.2s;
}
.preset-btn:hover {
  background: var(--md-primary-fg-color, #009688);
  color: #fff;
}
.calc-add-row {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 6px 14px;
  border: 1px dashed var(--md-primary-fg-color, #009688);
  border-radius: 6px;
  background: transparent;
  color: var(--md-primary-fg-color, #009688);
  cursor: pointer;
  font-size: 0.85em;
  margin-top: 8px;
}
.calc-add-row:hover {
  background: rgba(0, 150, 136, 0.1);
}
.workload-row {
  display: grid;
  grid-template-columns: 2fr 1fr 1fr 1fr 1fr 1fr auto;
  gap: 8px;
  align-items: end;
  margin-bottom: 8px;
}
@media (max-width: 768px) {
  .workload-row {
    grid-template-columns: 1fr 1fr;
  }
}
.workload-row input {
  padding: 8px 10px;
  border: 1px solid var(--md-default-fg-color--lightest, #ccc);
  border-radius: 6px;
  font-size: 0.9em;
  background: var(--md-default-bg-color, #fff);
  color: var(--md-default-fg-color, #333);
  width: 100%;
  box-sizing: border-box;
}
.workload-row input:focus {
  outline: none;
  border-color: var(--md-primary-fg-color, #009688);
}
.workload-header {
  display: grid;
  grid-template-columns: 2fr 1fr 1fr 1fr 1fr 1fr auto;
  gap: 8px;
  margin-bottom: 4px;
  font-size: 0.75em;
  font-weight: 600;
  color: var(--md-default-fg-color--light, #666);
  text-transform: uppercase;
}
@media (max-width: 768px) {
  .workload-header { display: none; }
}
.mobile-label {
  display: none;
  font-size: 0.7em;
  font-weight: 600;
  color: var(--md-default-fg-color--light, #666);
  text-transform: uppercase;
  margin-bottom: 2px;
}
@media (max-width: 768px) {
  .mobile-label { display: block; }
}
.remove-btn {
  background: none;
  border: none;
  color: #e53935;
  cursor: pointer;
  font-size: 1.2em;
  padding: 4px 8px;
  border-radius: 4px;
}
.remove-btn:hover {
  background: rgba(229, 57, 53, 0.1);
}
.note-box {
  background: rgba(255,255,255,0.1);
  border-radius: 6px;
  padding: 12px 16px;
  margin-top: 16px;
  font-size: 0.85em;
  line-height: 1.5;
}
.under-provisioned td {
  color: #ffcc80 !important;
}
.under-provisioned td:first-child::before {
  content: "\26A0\FE0F ";
}
.under-prov-note {
  background: rgba(255, 152, 0, 0.2);
  border-left: 3px solid #ffcc80;
  border-radius: 0 6px 6px 0;
  padding: 10px 14px;
  margin-top: 12px;
  font-size: 0.85em;
  line-height: 1.5;
  display: none;
}
</style>

<div class="calc-container">

<div class="calc-section">
<h3>Quick Presets</h3>
<p style="margin-top:0; font-size:0.9em;">Start with a typical scenario, then customize the numbers to match your environment.</p>
<div class="calc-presets">
  <button class="preset-btn" onclick="applyPreset('small')">Small team (5 services)</button>
  <button class="preset-btn" onclick="applyPreset('medium')">Mid-size (20 services)</button>
  <button class="preset-btn" onclick="applyPreset('large')">Large platform (100 services)</button>
  <button class="preset-btn" onclick="applyPreset('custom')">Start from scratch</button>
</div>
</div>

<div class="calc-section">
<h3>Cloud Pricing</h3>
<div class="calc-grid">
  <div class="calc-field">
    <label for="cpuPrice">CPU price ($/vCPU-hour)</label>
    <input type="number" id="cpuPrice" value="0.031" step="0.001" min="0" oninput="calculate()">
    <span class="help-text">AWS on-demand ~$0.031, GCP ~$0.034, Azure ~$0.036</span>
  </div>
  <div class="calc-field">
    <label for="memPrice">Memory price ($/GiB-hour)</label>
    <input type="number" id="memPrice" value="0.004" step="0.001" min="0" oninput="calculate()">
    <span class="help-text">AWS ~$0.004, GCP ~$0.005, Azure ~$0.004</span>
  </div>
  <div class="calc-field">
    <label for="overheadCpu">CPU overhead</label>
    <select id="overheadCpu" onchange="calculate()">
      <option value="10">10% headroom</option>
      <option value="20" selected>20% headroom — recommended</option>
      <option value="30">30% headroom</option>
      <option value="50">50% headroom (bursty workloads)</option>
    </select>
  </div>
  <div class="calc-field">
    <label for="overheadMem">Memory overhead</label>
    <select id="overheadMem" onchange="calculate()">
      <option value="10">10% headroom</option>
      <option value="20">20% headroom</option>
      <option value="30" selected>30% headroom — recommended</option>
      <option value="50">50% headroom</option>
    </select>
  </div>
</div>
</div>

<div class="calc-section">
<h3>Your Workloads</h3>
<p style="margin-top:0; font-size:0.9em;">Enter each service's current resource requests, actual P95 usage, and replica count.</p>
<div class="workload-header">
  <span>Service name</span>
  <span>CPU req (m)</span>
  <span>CPU P95 (m)</span>
  <span>Mem req (Mi)</span>
  <span>Mem P95 (Mi)</span>
  <span>Replicas</span>
  <span></span>
</div>
<div id="workloadRows"></div>
<button class="calc-add-row" onclick="addWorkloadRow('', 500, 100, 512, 150, 3)">+ Add workload</button>
</div>

<div class="calc-results" id="resultsSection">
<h3>Projected Savings with Attune</h3>
<div class="results-grid">
  <div class="result-card">
    <div class="result-label">Monthly savings</div>
    <div class="result-value" id="monthlySavings">$0</div>
  </div>
  <div class="result-card">
    <div class="result-label">Annual savings</div>
    <div class="result-value" id="annualSavings">$0</div>
  </div>
  <div class="result-card">
    <div class="result-label">Resource reduction</div>
    <div class="result-value" id="overallReduction">0%</div>
  </div>
</div>

<div class="savings-bar-container">
  <div class="savings-bar-label">
    <span>CPU utilization: <span id="cpuUtilBefore">0%</span> requested → <span id="cpuUtilAfter">0%</span> after right-sizing</span>
  </div>
  <div class="savings-bar">
    <div class="savings-bar-fill cpu" id="cpuBar" style="width: 0%"></div>
  </div>
</div>
<div class="savings-bar-container" style="margin-top: 8px;">
  <div class="savings-bar-label">
    <span>Memory utilization: <span id="memUtilBefore">0%</span> requested → <span id="memUtilAfter">0%</span> after right-sizing</span>
  </div>
  <div class="savings-bar">
    <div class="savings-bar-fill mem" id="memBar" style="width: 0%"></div>
  </div>
</div>

<table class="breakdown-table" id="breakdownTable">
  <thead>
    <tr>
      <th>Service</th>
      <th>CPU: current → right-sized</th>
      <th>Memory: current → right-sized</th>
      <th>Monthly savings</th>
    </tr>
  </thead>
  <tbody id="breakdownBody"></tbody>
</table>

<div class="under-prov-note" id="underProvNote">
  <strong>Under-provisioned workloads detected.</strong> Rows marked with a
  warning have P95 usage that exceeds the current resource request (after
  applying the overhead). Right-sizing these workloads would
  <em>increase</em> their requests, preventing throttling and OOMKills. This
  improves reliability rather than reducing cost.
</div>

<div class="note-box">
  <strong>How this is calculated:</strong> For each workload, the right-sized
  value is <code>P95_usage x (1 + overhead/100)</code>. Monthly cost uses
  <code>(cores x CPU_price + GiB x mem_price) x 730 hours</code>. Savings are
  the difference between current and right-sized costs across all replicas.
  Actual savings may be higher due to improved bin-packing enabling node
  consolidation.
</div>
</div>

</div>

<script>
let workloadId = 0;

function addWorkloadRow(name, cpuReq, cpuP95, memReq, memP95, replicas) {
  const container = document.getElementById('workloadRows');
  const id = workloadId++;
  const div = document.createElement('div');
  div.className = 'workload-row';
  div.id = 'row-' + id;
  div.innerHTML = `
    <div><span class="mobile-label">Service name</span><input type="text" value="${name}" placeholder="e.g. api-server" oninput="calculate()"></div>
    <div><span class="mobile-label">CPU req (m)</span><input type="number" value="${cpuReq}" min="0" step="10" placeholder="CPU req (m)" oninput="calculate()"></div>
    <div><span class="mobile-label">CPU P95 (m)</span><input type="number" value="${cpuP95}" min="0" step="10" placeholder="CPU P95 (m)" oninput="calculate()"></div>
    <div><span class="mobile-label">Mem req (Mi)</span><input type="number" value="${memReq}" min="0" step="16" placeholder="Mem req (Mi)" oninput="calculate()"></div>
    <div><span class="mobile-label">Mem P95 (Mi)</span><input type="number" value="${memP95}" min="0" step="16" placeholder="Mem P95 (Mi)" oninput="calculate()"></div>
    <div><span class="mobile-label">Replicas</span><input type="number" value="${replicas}" min="1" step="1" placeholder="Replicas" oninput="calculate()"></div>
    <button class="remove-btn" onclick="removeRow(${id})" title="Remove">&times;</button>
  `;
  container.appendChild(div);
  calculate();
}

function removeRow(id) {
  const row = document.getElementById('row-' + id);
  if (row) row.remove();
  calculate();
}

function getWorkloads() {
  const rows = document.querySelectorAll('.workload-row');
  const workloads = [];
  rows.forEach(row => {
    const inputs = row.querySelectorAll('input');
    workloads.push({
      name: inputs[0].value || 'unnamed',
      cpuReq: parseFloat(inputs[1].value) || 0,
      cpuP95: parseFloat(inputs[2].value) || 0,
      memReq: parseFloat(inputs[3].value) || 0,
      memP95: parseFloat(inputs[4].value) || 0,
      replicas: parseInt(inputs[5].value) || 1
    });
  });
  return workloads;
}

function calculate() {
  const cpuPrice = parseFloat(document.getElementById('cpuPrice').value) || 0;
  const memPrice = parseFloat(document.getElementById('memPrice').value) || 0;
  const cpuMargin = 1 + parseFloat(document.getElementById('overheadCpu').value) / 100;
  const memMargin = 1 + parseFloat(document.getElementById('overheadMem').value) / 100;
  const hoursPerMonth = 730;

  const workloads = getWorkloads();
  let totalMonthlySavings = 0;
  let totalCpuReq = 0, totalCpuP95 = 0, totalMemReq = 0, totalMemP95 = 0;
  let totalCurrentCost = 0, totalNewCost = 0;
  const breakdown = [];

  workloads.forEach(w => {
    const cpuTuned = Math.max(w.cpuP95 * cpuMargin, w.cpuP95);
    const memTuned = Math.max(w.memP95 * memMargin, w.memP95);

    const cpuCurrentCost = (w.cpuReq / 1000) * cpuPrice * hoursPerMonth * w.replicas;
    const memCurrentCost = (w.memReq / 1024) * memPrice * hoursPerMonth * w.replicas;
    const cpuNewCost = (Math.min(cpuTuned, w.cpuReq) / 1000) * cpuPrice * hoursPerMonth * w.replicas;
    const memNewCost = (Math.min(memTuned, w.memReq) / 1024) * memPrice * hoursPerMonth * w.replicas;

    const monthlySaved = (cpuCurrentCost + memCurrentCost) - (cpuNewCost + memNewCost);
    totalMonthlySavings += monthlySaved;
    totalCurrentCost += cpuCurrentCost + memCurrentCost;
    totalNewCost += cpuNewCost + memNewCost;

    totalCpuReq += w.cpuReq * w.replicas;
    totalCpuP95 += w.cpuP95 * w.replicas;
    totalMemReq += w.memReq * w.replicas;
    totalMemP95 += w.memP95 * w.replicas;

    const cpuUnderProv = cpuTuned > w.cpuReq && w.cpuReq > 0;
    const memUnderProv = memTuned > w.memReq && w.memReq > 0;

    breakdown.push({
      name: w.name,
      cpuReq: w.cpuReq,
      cpuNew: Math.round(cpuUnderProv ? cpuTuned : Math.min(cpuTuned, w.cpuReq)),
      memReq: w.memReq,
      memNew: Math.round(memUnderProv ? memTuned : Math.min(memTuned, w.memReq)),
      saved: monthlySaved,
      underProv: cpuUnderProv || memUnderProv
    });
  });

  document.getElementById('monthlySavings').textContent =
    '$' + Math.round(totalMonthlySavings).toLocaleString();
  document.getElementById('annualSavings').textContent =
    '$' + Math.round(totalMonthlySavings * 12).toLocaleString();

  const reduction = totalCurrentCost > 0
    ? Math.round(((totalCurrentCost - totalNewCost) / totalCurrentCost) * 100)
    : 0;
  document.getElementById('overallReduction').textContent = reduction + '%';

  const cpuUtilBefore = totalCpuReq > 0 ? Math.round((totalCpuP95 / totalCpuReq) * 100) : 0;
  const cpuTunedTotal = totalCpuP95 * cpuMargin;
  const cpuUtilAfter = cpuTunedTotal > 0
    ? Math.min(100, Math.round((totalCpuP95 / Math.min(cpuTunedTotal, totalCpuReq)) * 100))
    : 0;
  document.getElementById('cpuUtilBefore').textContent = cpuUtilBefore + '%';
  document.getElementById('cpuUtilAfter').textContent = cpuUtilAfter + '%';
  document.getElementById('cpuBar').style.width = cpuUtilAfter + '%';

  const memUtilBefore = totalMemReq > 0 ? Math.round((totalMemP95 / totalMemReq) * 100) : 0;
  const memTunedTotal = totalMemP95 * memMargin;
  const memUtilAfter = memTunedTotal > 0
    ? Math.min(100, Math.round((totalMemP95 / Math.min(memTunedTotal, totalMemReq)) * 100))
    : 0;
  document.getElementById('memUtilBefore').textContent = memUtilBefore + '%';
  document.getElementById('memUtilAfter').textContent = memUtilAfter + '%';
  document.getElementById('memBar').style.width = memUtilAfter + '%';

  const tbody = document.getElementById('breakdownBody');
  tbody.innerHTML = '';
  let hasUnderProv = false;
  breakdown.forEach(b => {
    const tr = document.createElement('tr');
    if (b.underProv) {
      tr.className = 'under-provisioned';
      hasUnderProv = true;
    }
    const cpuArrow = b.cpuNew > b.cpuReq ? '\u2191' : '\u2192';
    const memArrow = b.memNew > b.memReq ? '\u2191' : '\u2192';
    const savingsText = b.underProv && b.saved === 0
      ? 'needs more resources'
      : '$' + Math.round(b.saved).toLocaleString() + '/mo';
    tr.innerHTML = `
      <td>${b.name}</td>
      <td>${b.cpuReq}m ${cpuArrow} ${b.cpuNew}m</td>
      <td>${b.memReq}Mi ${memArrow} ${b.memNew}Mi</td>
      <td><strong>${savingsText}</strong></td>
    `;
    tbody.appendChild(tr);
  });

  if (breakdown.length > 0) {
    const totalRow = document.createElement('tr');
    totalRow.innerHTML = `
      <td><strong>Total</strong></td>
      <td></td>
      <td></td>
      <td><strong>$${Math.round(totalMonthlySavings).toLocaleString()}/mo</strong></td>
    `;
    tbody.appendChild(totalRow);
  }

  document.getElementById('underProvNote').style.display =
    hasUnderProv ? 'block' : 'none';
}

function applyPreset(type) {
  document.getElementById('workloadRows').innerHTML = '';
  workloadId = 0;

  if (type === 'small') {
    addWorkloadRow('api-gateway', 1000, 200, 1024, 300, 3);
    addWorkloadRow('user-service', 500, 80, 512, 150, 2);
    addWorkloadRow('order-service', 500, 120, 512, 200, 2);
    addWorkloadRow('notification-svc', 250, 30, 256, 80, 2);
    addWorkloadRow('worker', 1000, 150, 2048, 400, 2);
  } else if (type === 'medium') {
    addWorkloadRow('api-gateway', 2000, 400, 2048, 600, 5);
    addWorkloadRow('user-service', 1000, 150, 1024, 300, 3);
    addWorkloadRow('order-service', 1000, 200, 1024, 350, 4);
    addWorkloadRow('payment-service', 500, 100, 512, 200, 3);
    addWorkloadRow('inventory-svc', 500, 80, 512, 180, 3);
    addWorkloadRow('notification-svc', 500, 50, 512, 100, 2);
    addWorkloadRow('search-service', 2000, 600, 4096, 1200, 3);
    addWorkloadRow('recommendation', 1000, 200, 2048, 500, 2);
    addWorkloadRow('auth-service', 500, 60, 256, 80, 3);
    addWorkloadRow('email-worker', 500, 40, 512, 100, 2);
    addWorkloadRow('report-generator', 2000, 300, 4096, 800, 2);
    addWorkloadRow('cache-warmer', 500, 50, 1024, 200, 2);
    addWorkloadRow('data-pipeline', 1000, 400, 2048, 600, 3);
    addWorkloadRow('metrics-agg', 500, 100, 1024, 300, 2);
    addWorkloadRow('frontend-ssr', 500, 80, 512, 150, 4);
    addWorkloadRow('cdn-origin', 500, 60, 256, 100, 2);
    addWorkloadRow('webhook-handler', 250, 30, 256, 60, 2);
    addWorkloadRow('scheduler', 500, 50, 512, 120, 1);
    addWorkloadRow('migration-runner', 1000, 50, 1024, 200, 1);
    addWorkloadRow('healthcheck-svc', 250, 20, 128, 40, 2);
  } else if (type === 'large') {
    addWorkloadRow('api-gateway', 4000, 800, 4096, 1200, 10);
    addWorkloadRow('user-service', 2000, 300, 2048, 600, 8);
    addWorkloadRow('order-service', 2000, 400, 2048, 700, 8);
    addWorkloadRow('payment-service', 1000, 200, 1024, 400, 5);
    addWorkloadRow('search-service', 4000, 1200, 8192, 2400, 6);
    addWorkloadRow('recommendation', 2000, 500, 4096, 1000, 4);
    addWorkloadRow('ml-inference', 4000, 800, 8192, 2000, 4);
    addWorkloadRow('data-pipeline', 2000, 600, 4096, 1200, 6);
    addWorkloadRow('70+ other services (avg)', 500, 100, 512, 150, 210);
  } else {
    addWorkloadRow('', 500, 100, 512, 150, 3);
  }
  calculate();
}

// Initialize with small preset
document.addEventListener('DOMContentLoaded', function() {
  applyPreset('small');
});
// Also run immediately in case DOMContentLoaded already fired
if (document.readyState !== 'loading') {
  applyPreset('small');
}
</script>

---

## Understanding the Numbers

### How right-sized values are calculated

For each workload, the **right-sized resource request** is:

```
right_sized = P95_usage x (1 + overhead/100)
```

This matches the operator's default algorithm: take the 95th percentile of
observed CPU usage (or 99th for memory), multiply by the overhead, and
clamp to the configured bounds.

### Why actual savings may be higher

This calculator computes **direct resource savings**, the difference between
what you're requesting now and what you'd request after right-sizing. But
the real impact goes further:

- **Node consolidation**: Lower pod requests mean better bin-packing. Your
  cluster autoscaler (or Karpenter) can fit more pods per node and scale
  down unused nodes. This often adds 10-30% on top of the per-pod savings.

- **Reduced spot/reserved waste**: Right-sized workloads let you buy smaller
  reserved instances or committed use discounts, matching actual need instead
  of peak overestimate.

- **Operational time saved**: No more quarterly resource review meetings. No
  more "why is the cluster full when utilization is 8%" investigations.

### Common findings

| Cluster size | Typical monthly waste | Typical reduction |
|-------------|----------------------|-------------------|
| 5-10 services | $200-1,000 | 40-70% |
| 20-50 services | $2,000-10,000 | 50-75% |
| 100+ services | $15,000-100,000+ | 40-65% |

These ranges are based on industry benchmarks from
[CAST AI](https://cast.ai/reports/state-of-kubernetes-optimization/),
[Datadog](https://www.datadoghq.com/state-of-cloud-costs/), and
[ScaleOps](https://scaleops.com/blog/why-pod-rightsizing-fails-in-production-a-deep-dive-into-vpa-and-what-actually-works/)
reports.

---

**Ready to capture these savings?**

- [Install Attune](getting-started/installation.md) in 5 minutes
- [Start with Recommend mode](getting-started/quickstart.md) to validate
  the numbers in your own cluster
- [Read why Attune](why-attune.md) for the full story
