// Quarter shortcut buttons + sessionStorage persistence for from/to dates.
// Looks for inputs named "from" and "to" inside any form, and any element
// with [data-quarter] / [data-quarter-year] inside that form.
(function () {
  const KEY_FROM = 'usageFrom';
  const KEY_TO = 'usageTo';

  // Exposed so dashboard's Reset can drop the persisted range too.
  window.clearPersistedDates = function () {
    sessionStorage.removeItem(KEY_FROM);
    sessionStorage.removeItem(KEY_TO);
  };

  const fromInput = document.querySelector('input[name=from]');
  const toInput = document.querySelector('input[name=to]');
  if (!fromInput || !toInput) return;

  const form = fromInput.form;
  const url = new URL(location.href);
  const urlFrom = url.searchParams.get('from');
  const urlTo = url.searchParams.get('to');

  // 1. Persist whatever just came in via URL.
  if (urlFrom) sessionStorage.setItem(KEY_FROM, urlFrom);
  if (urlTo) sessionStorage.setItem(KEY_TO, urlTo);

  // 2. If neither is in the URL, restore from sessionStorage. On the
  //    server-rendered report page, redirect so the page reloads with
  //    the persisted range; on the dashboard (no URL params used) just
  //    overwrite the input values.
  // The report page is server-rendered — to apply the persisted range we
  // have to reload with from/to in the query string. The dashboard renders
  // its filters in JS, so just overwrite the input values.
  const serverRendered = form && form.getAttribute('action');
  if (!urlFrom && !urlTo) {
    const sf = sessionStorage.getItem(KEY_FROM);
    const st = sessionStorage.getItem(KEY_TO);
    if (sf || st) {
      if (serverRendered) {
        const params = new URLSearchParams(location.search);
        if (sf) params.set('from', sf);
        if (st) params.set('to', st);
        location.replace(location.pathname + '?' + params.toString());
        return;
      }
      if (sf) fromInput.value = sf;
      if (st) toInput.value = st;
    }
  }

  // 3. Save on submit so the next page sees the latest pick.
  if (form) {
    form.addEventListener('submit', () => {
      if (fromInput.value) sessionStorage.setItem(KEY_FROM, fromInput.value);
      if (toInput.value) sessionStorage.setItem(KEY_TO, toInput.value);
    });
  }

  // 4. Wire up quarter buttons.
  const yearSel = document.querySelector('[data-quarter-year]');
  if (yearSel && yearSel.options.length === 0) {
    const cur = new Date().getFullYear();
    for (let y = cur; y >= cur - 3; y--) {
      const o = document.createElement('option');
      o.value = String(y);
      o.textContent = String(y);
      yearSel.appendChild(o);
    }
  }

  function pad(n) { return n < 10 ? '0' + n : '' + n; }
  function ymd(y, m, d) { return y + '-' + pad(m) + '-' + pad(d); }

  document.querySelectorAll('[data-quarter]').forEach((btn) => {
    btn.addEventListener('click', () => {
      const q = parseInt(btn.dataset.quarter, 10);
      const y = parseInt((yearSel && yearSel.value) || new Date().getFullYear(), 10);
      const startMonth = (q - 1) * 3 + 1;
      const endMonth = startMonth + 2;
      const lastDay = new Date(y, endMonth, 0).getDate();
      fromInput.value = ymd(y, startMonth, 1);
      toInput.value = ymd(y, endMonth, lastDay);
      if (form) form.requestSubmit ? form.requestSubmit() : form.submit();
    });
  });
})();
