// Readable end-of-run summary.
//
// k6's default summary buries the custom counters among its built-ins. This
// prints the metrics that carry the invariants first, then the standard block.
export function textSummary(data, opts) {
  const title = (opts && opts.title) || 'load';
  const lines = [];
  const pad = (s, n) => String(s).padEnd(n);

  lines.push('');
  lines.push(`===== ${title} =====`);

  const m = data.metrics || {};
  const get = (name, field, fallback = '-') => {
    const metric = m[name];
    if (!metric || metric.values[field] === undefined) return fallback;
    return metric.values[field];
  };
  const rate = (name) => {
    const v = get(name, 'rate', null);
    return v === null ? '-' : `${(v * 100).toFixed(2)}%`;
  };
  const trend = (name, field, unit = 'ms') => {
    const v = get(name, field, null);
    return v === null ? '-' : `${Number(v).toFixed(2)}${unit}`;
  };
  const count = (name) => {
    const metric = m[name];
    if (!metric) return '-';
    const v = metric.values;
    if (v.count !== undefined) return v.count; // counter
    if (v.value !== undefined) return v.value; // gauge
    return '-';
  };

  // Invariant-bearing counters first.
  const interesting = [
    'hot_applied',
    'hot_replayed',
    'hot_conflicts',
    'idem_applied',
    'idem_replayed',
    'idem_conflicts',
    'idem_rejected',
    'mixed_applied',
    'mixed_replayed',
    'mixed_rejected_insufficient',
    'mixed_rejected_reference',
    'mixed_duplicate_wallet',
    'mixed_not_found',
    'mixed_server_errors',
    'soak_applied',
    'soak_outbox_pending',
    'dist_applied',
    'dist_conflicts',
  ];
  const present = interesting.filter((k) => m[k]);

  if (present.length > 0) {
    lines.push('');
    lines.push('-- counters --');
    for (const k of present) {
      lines.push(`  ${pad(k, 30)} ${count(k)}`);
    }
  }

  lines.push('');
  lines.push('-- correctness --');
  lines.push(`  ${pad('checks', 30)} ${rate('checks')}`);
  lines.push(`  ${pad('http_req_failed', 30)} ${rate('http_req_failed')}`);

  // Which check failed matters more than the percentage: a bare "95%" hides the
  // one assertion that regressed. In handleSummary's payload `checks` is a LIST;
  // guard for the dict shape too so the helper is safe either way.
  const failedChecks = [];
  const asEntries = (checks) =>
    Array.isArray(checks) ? checks.map((c) => [c.name, c]) : Object.entries(checks || {});
  const walk = (group, path) => {
    for (const [name, c] of asEntries(group.checks)) {
      if (c.fails > 0) failedChecks.push({ name: path ? `${path} / ${name}` : name, fails: c.fails, passes: c.passes });
    }
    for (const sub of group.groups || []) walk(sub, path ? `${path} / ${sub.name}` : sub.name);
  };
  if (data.root_group) walk(data.root_group, '');
  if (failedChecks.length > 0) {
    lines.push('');
    lines.push('-- failed checks --');
    for (const c of failedChecks) {
      lines.push(`  ✗ ${c.fails} failed / ${c.passes} passed — ${c.name}`);
    }
  }

  lines.push('');
  lines.push('-- latency --');
  for (const [name, label] of [
    ['wallet_money_latency', 'money path'],
    ['wallet_read_latency', 'read path'],
    ['http_req_duration', 'all requests'],
  ]) {
    if (!m[name]) continue;
    // A trend with no samples would render as "0.00ms" and look like a
    // measurement rather than an absence. Trends expose no `count` (only
    // percentiles), so presence of a percentile is the signal to use.
    if (get(name, 'p(95)', null) === null && get(name, 'max', null) === null) continue;
    lines.push(
      `  ${pad(label, 30)} p50 ${trend(name, 'med')}  p95 ${trend(name, 'p(95)')}  p99 ${trend(
        name,
        'p(99)'
      )}`
    );
  }

  lines.push('');
  lines.push('-- throughput --');
  lines.push(`  ${pad('requests', 30)} ${count('http_reqs')}`);
  lines.push(`  ${pad('iterations', 30)} ${count('iterations')}`);
  lines.push(`  ${pad('request rate', 30)} ${Number(get('http_reqs', 'rate', 0)).toFixed(1)}/s`);

  const failed = Object.entries(data.metrics || {})
    .filter(([, metric]) => metric.thresholds)
    .flatMap(([name, metric]) =>
      Object.entries(metric.thresholds)
        .filter(([, t]) => t.ok === false)
        .map(([expr]) => `${name}: ${expr}`)
    );

  lines.push('');
  if (failed.length > 0) {
    lines.push('-- THRESHOLDS FAILED --');
    for (const f of failed) lines.push(`  ✗ ${f}`);
  } else {
    lines.push('-- all thresholds passed --');
  }
  lines.push('');

  return lines.join('\n');
}
