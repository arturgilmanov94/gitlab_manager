/* Dashboard helpers: JSON API calls, toast, tooltips, overflow menus, run polling, elapsed timers. */
(function () {
  // ---- toast / feedback
  let toastTimer = null;
  window.flash = function (message, ok) {
    let el = document.getElementById('toast');
    if (!el) { el = document.createElement('div'); el.id = 'toast'; document.body.appendChild(el); }
    el.className = 'toast' + (ok ? '' : ' error');
    el.textContent = message;
    el.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { el.hidden = true; }, ok ? 3500 : 8000);
  };
  window.reload = () => { try { sessionStorage.setItem('scroll:' + location.pathname, String(window.scrollY)); } catch (e) {} window.location.reload(); };
  window.go = (url) => { window.location.href = url; };
  try {
    const saved = sessionStorage.getItem('scroll:' + location.pathname);
    if (saved) { sessionStorage.removeItem('scroll:' + location.pathname); window.scrollTo(0, parseInt(saved, 10) || 0); }
  } catch (e) { /* ignore */ }

  function busy(button, on) {
    if (!button || !button.classList) return;
    button.disabled = on;
    button.classList.toggle('busy', on);
  }

  async function call(method, url, body, button) {
    busy(button, true);
    try {
      const response = await fetch(url, {
        method,
        headers: { 'Content-Type': 'application/json' },
        body: body === undefined ? undefined : JSON.stringify(body),
      });
      const data = await response.json().catch(() => ({}));
      if (!response.ok || data.ok === false) throw new Error(data.error || response.statusText);
      return data;
    } catch (error) {
      flash(error.message || String(error), false);
      return null;
    } finally {
      busy(button, false);
    }
  }
  window.post = (url, body, button) => call('POST', url, body || {}, button);
  window.del = (url, button) => call('DELETE', url, undefined, button);

  // ---- agent picker
  function selectedRunner() {
    const checked = document.querySelector('input[name="runner"]:checked');
    return checked ? checked.value : '';
  }
  document.querySelectorAll('.segmented').forEach((group) => {
    group.addEventListener('change', () => {
      group.querySelectorAll('.seg').forEach((seg) => seg.classList.toggle('checked', seg.querySelector('input').checked));
    });
  });

  // ---- overflow menus: close on outside click / Escape
  document.addEventListener('click', (event) => {
    document.querySelectorAll('details.menu[open]').forEach((menu) => {
      if (!menu.contains(event.target)) menu.removeAttribute('open');
    });
  });
  document.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') document.querySelectorAll('details.menu[open]').forEach((m) => m.removeAttribute('open'));
  });

  // ---- actions
  window.addRef = async function (event, url) {
    event.preventDefault();
    const input = event.target.querySelector('input[name="ref"]');
    const data = await call('POST', url, { url: input.value }, event.submitter);
    if (data && data.redirect) go(data.redirect);
    return false;
  };

  const startedLabels = {
    quick: 'Быстрое ревью поставлено в очередь', full: 'Полное ревью поставлено в очередь', verify: 'Проверка изменений поставлена в очередь',
    fix: 'Исправление замечаний запущено, создаю workspace', plan: 'Исследование поставлено в очередь', implement: 'Решение задачи запущено, создаю workspace',
  };
  // Start a run and stay on the current page: the row/page shows the new state, several runs can be
  // started from a list one after another. The run page is one click away («Открыть прогресс»).
  window.startRun = async function (url, kind, button, extra) {
    const body = Object.assign({ kind, runner: selectedRunner() }, extra || {});
    const data = await call('POST', url, body, button);
    if (data && data.redirect) { flash(startedLabels[kind] || 'Запуск поставлен в очередь', true); setTimeout(reload, 500); }
  };

  // ---- client-side filter for lists (input[data-filter="<table selector>"])
  document.querySelectorAll('input[data-filter]').forEach((input) => {
    const rows = () => Array.from(document.querySelectorAll(input.dataset.filter + ' tbody tr'));
    const apply = () => {
      const q = input.value.trim().toLowerCase();
      let shown = 0;
      rows().forEach((row) => { const hit = !q || row.textContent.toLowerCase().includes(q); row.hidden = !hit; if (hit) shown++; });
      const counter = document.getElementById('filter-count');
      if (counter) counter.textContent = q ? `${shown} из ${rows().length}` : '';
    };
    input.addEventListener('input', apply);
    try { const saved = sessionStorage.getItem('filter:' + location.pathname); if (saved) { input.value = saved; apply(); } } catch (e) { /* ignore */ }
    input.addEventListener('input', () => { try { sessionStorage.setItem('filter:' + location.pathname, input.value); } catch (e) { /* ignore */ } });
  });

  // ---- technical log: light syntax colouring by line prefix
  const logClasses = [
    [/^\[tool\]/, 'l-tool'], [/^\[permission\?\]/, 'l-ask'], [/^\[permission\]/, 'l-perm'], [/^\[denied\]/, 'l-denied'],
    [/^\[init\]/, 'l-init'], [/^\[\d{4}-\d\d-\d\dT/, 'l-time'], [/^\$ /, 'l-cmd'], [/^--- /, 'l-sep'],
  ];
  function formatLog(text) {
    return text.split('\n').map((line) => {
      const safe = line.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
      const cls = (logClasses.find(([re]) => re.test(line)) || [])[1];
      return cls ? `<span class="${cls}">${safe}</span>` : safe;
    }).join('\n');
  }
  document.querySelectorAll('pre.log[data-format="log"]').forEach((pre) => { pre.innerHTML = formatLog(pre.textContent); pre.scrollTop = pre.scrollHeight; });

  window.ask = async function (event, runId) {
    event.preventDefault();
    const area = document.getElementById('question');
    const chat = document.getElementById('chat');
    const question = area.value.trim();
    if (!question) return false;
    const pending = document.createElement('div');
    pending.className = 'msg msg-user';
    pending.innerHTML = '<div class="msg-role">вы</div><div class="prewrap"></div>';
    pending.querySelector('.prewrap').textContent = question;
    chat.appendChild(pending);
    const waiting = document.createElement('div');
    waiting.className = 'msg msg-assistant';
    waiting.innerHTML = '<div class="msg-role"><span class="spinner"></span> агент отвечает в той же сессии…</div>';
    chat.appendChild(waiting);
    area.value = '';
    const data = await call('POST', `/api/runs/${runId}/ask`, { question }, event.submitter);
    if (data) {
      waiting.innerHTML = '<div class="msg-role">агент</div><div class="prewrap"></div>';
      waiting.querySelector('.prewrap').textContent = data.answer;
    } else {
      waiting.remove();
    }
    return false;
  };

  window.createMR = async function (runId, button) {
    const title = prompt('Заголовок MR (пусто = по задаче):', '');
    if (title === null) return;
    const description = prompt('Описание MR (пусто = ссылка на задачу):', '');
    if (description === null) return;
    const data = await call('POST', `/api/runs/${runId}/mr`, { title, description }, button);
    if (data && data.url) { flash('MR создан: ' + data.url, true); window.open(data.url, '_blank'); }
  };

  // Answer a permission prompt of a waiting run (allow | allow_always | deny).
  window.decide = async function (approvalId, decision, button) {
    const noteEl = document.getElementById('deny-note');
    const note = decision === 'deny' && noteEl ? noteEl.value : '';
    const data = await call('POST', `/api/approvals/${approvalId}`, { decision, note }, button);
    if (data) {
      flash(decision === 'deny' ? 'Отклонено, агент продолжает' : 'Разрешено, агент продолжает', true);
      setTimeout(reload, 600);
    }
  };

  window.copyText = async function (text) {
    try { await navigator.clipboard.writeText(text); flash('Скопировано', true); } catch (e) { flash('Не удалось скопировать', false); }
  };

  // ---- tooltips (also for disabled controls wrapped in .disabled-wrap)
  const tooltip = document.getElementById('tooltip');
  if (tooltip) {
    let current = null;
    document.addEventListener('mouseover', (event) => {
      const target = event.target.closest('[data-tip]');
      if (!target || target === current) return;
      current = target;
      tooltip.textContent = target.dataset.tip;
      tooltip.hidden = false;
      const rect = target.getBoundingClientRect();
      tooltip.style.maxWidth = Math.min(360, window.innerWidth - 24) + 'px';
      let left = rect.left;
      if (left + tooltip.offsetWidth > window.innerWidth - 12) left = window.innerWidth - tooltip.offsetWidth - 12;
      let top = rect.bottom + 8;
      if (top + tooltip.offsetHeight > window.innerHeight - 8) top = rect.top - tooltip.offsetHeight - 8;
      tooltip.style.left = Math.max(8, left) + 'px';
      tooltip.style.top = Math.max(8, top) + 'px';
    });
    document.addEventListener('mouseout', (event) => {
      if (current && !current.contains(event.relatedTarget)) { tooltip.hidden = true; current = null; }
    });
  }

  // ---- elapsed timers
  function tickElapsed() {
    document.querySelectorAll('[data-started]').forEach((box) => {
      const el = box.querySelector('.elapsed');
      const started = Date.parse(box.dataset.started);
      if (!el || isNaN(started)) return;
      const sec = Math.max(0, Math.floor((Date.now() - started) / 1000));
      el.textContent = `${Math.floor(sec / 60)}:${String(sec % 60).padStart(2, '0')}`;
    });
  }
  tickElapsed();
  setInterval(tickElapsed, 1000);

  // ---- poll active runs; reload when a status changes (finished, or the agent asks for a permission)
  const active = Array.from(document.querySelectorAll('[data-run-status]')).filter((el) => /queued|running|waiting/.test(el.dataset.status));
  if (active.length) {
    const ids = Array.from(new Set(active.map((el) => el.dataset.runStatus)));
    const initial = {};
    active.forEach((el) => { initial[el.dataset.runStatus] = el.dataset.status; });
    const logTail = document.getElementById('log-tail');
    const progressNow = document.getElementById('progress-now');
    const tick = async () => {
      let changed = false;
      for (const id of ids) {
        try {
          const response = await fetch(`/api/runs/${id}`);
          if (!response.ok) continue;
          const data = await response.json();
          if (data.status !== initial[id]) changed = true;
          if (progressNow && ids.length === 1 && data.progress) progressNow.textContent = 'сейчас: ' + data.progress;
        } catch (error) { /* server restarting */ }
      }
      if (changed) { reload(); return; }
      if (logTail && ids.length === 1) {
        try {
          const text = await (await fetch(`/run/${ids[0]}/log`)).text();
          logTail.innerHTML = formatLog(text.slice(-12000));
          logTail.scrollTop = logTail.scrollHeight;
        } catch (error) { /* ignore */ }
      }
      setTimeout(tick, 4000);
    };
    setTimeout(tick, 4000);
  }
})();
