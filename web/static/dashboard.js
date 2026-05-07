(function () {
  const form = document.getElementById('filters');
  const minRange = form.querySelector('input[name=min_duration]');
  const minLabel = document.getElementById('minLabel');
  const minUsersRange = form.querySelector('input[name=min_users]');
  const minUsersLabel = document.getElementById('minUsersLabel');

  const palette = [
    '#2563eb', '#dc2626', '#059669', '#d97706', '#7c3aed',
    '#0891b2', '#db2777', '#65a30d', '#ea580c', '#475569',
  ];
  const colorFor = (i) => palette[i % palette.length];

  let chart;

  function updateMinLabel() {
    const v = parseInt(minRange.value, 10);
    minLabel.textContent = v < 60 ? `${v}s` : `${Math.round(v / 60)}m`;
  }
  minRange.addEventListener('input', updateMinLabel);
  updateMinLabel();

  function updateMinUsersLabel() {
    minUsersLabel.textContent = `${parseInt(minUsersRange.value, 10)}+`;
  }
  minUsersRange.addEventListener('input', updateMinUsersLabel);
  updateMinUsersLabel();

  document.getElementById('resetBtn').addEventListener('click', () => {
    if (window.clearPersistedDates) window.clearPersistedDates();
    form.reset();
    updateMinLabel();
    updateMinUsersLabel();
    load();
  });

  document.getElementById('reportBtn').addEventListener('click', () => {
    location.href = '/report?' + buildQuery();
  });

  form.addEventListener('submit', (e) => {
    e.preventDefault();
    load();
  });

  function buildQuery() {
    const data = new FormData(form);
    const params = new URLSearchParams();
    for (const [k, v] of data.entries()) {
      if (v === '' || v == null) continue;
      params.append(k, v);
    }
    return params.toString();
  }

  function fmtSecs(s) {
    if (s < 60) return s + 's';
    if (s < 3600) return Math.round(s / 60) + 'm';
    return (s / 3600).toFixed(1) + 'h';
  }

  async function load() {
    const qs = buildQuery();
    const [usageRes, summaryRes] = await Promise.all([
      fetch('/api/usage?' + qs),
      fetch('/api/summary?' + qs),
    ]);
    if (usageRes.status === 401) { location.href = '/login'; return; }
    const usage = await usageRes.json();
    const summary = await summaryRes.json();

    document.getElementById('statSessions').textContent = (summary.total_sessions || 0).toLocaleString();
    document.getElementById('statHours').textContent = ((summary.total_seconds || 0) / 3600).toFixed(1);
    document.getElementById('statUsers').textContent = summary.unique_users || 0;
    document.getElementById('statDBs').textContent = summary.unique_dbs || 0;

    drawChart(usage.points || [], usage.bucket || 'day');
  }

  const bucketLabels = {
    '2h': '2-hour periods',
    'day': 'Day',
    'week': 'Week (Mon)',
    'month': 'Month',
  };

  function drawChart(points, bucket) {
    const days = [...new Set(points.map(p => p.day))].sort();
    const groups = [...new Set(points.map(p => p.group))].sort();
    const map = new Map();
    for (const p of points) map.set(p.day + '\0' + p.group, p.seconds);

    const datasets = groups.map((g, i) => ({
      label: g,
      data: days.map(d => ((map.get(d + '\0' + g) || 0) / 3600)),
      backgroundColor: colorFor(i),
      stack: 'usage',
    }));

    const ctx = document.getElementById('chart').getContext('2d');
    if (chart) chart.destroy();
    chart = new Chart(ctx, {
      type: 'bar',
      data: { labels: days, datasets },
      options: {
        responsive: true,
        plugins: {
          legend: { position: 'bottom' },
          tooltip: {
            callbacks: {
              label: (c) => `${c.dataset.label}: ${c.parsed.y.toFixed(2)} h`,
            },
          },
        },
        scales: {
          x: { stacked: true, title: { display: true, text: bucketLabels[bucket] || 'Day' } },
          y: { stacked: true, beginAtZero: true, title: { display: true, text: 'Hours' } },
        },
      },
    });
  }

  // Default: last 30 days
  if (!form.from.value) {
    const today = new Date();
    const from = new Date(today);
    from.setDate(today.getDate() - 30);
    form.from.value = from.toISOString().slice(0, 10);
    form.to.value = today.toISOString().slice(0, 10);
  }

  load();
})();
