/* Dashboard helpers: JSON API calls, tooltips, run polling, elapsed timers. */
(function () {
  const flashEl = document.getElementById('flash');
  window.flash = function (message, ok) {
    if (!flashEl) return;
    flashEl.textContent = message;
    flashEl.className = 'alert' + (ok ? ' ok' : '');
    flashEl.hidden = false;
  };
  window.reload = () => window.location.reload();
  window.go = (url) => { window.location.href = url; };

  function busy(button, on) {
    if (!button) return;
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

  function selectedRunner() {
    const checked = document.querySelector('input[name="runner"]:checked');
    return checked ? checked.value : '';
  }
  document.querySelectorAll('.segmented').forEach((group) => {
    group.addEventListener('change', () => {
      group.querySelectorAll('.seg').forEach((seg) => seg.classList.toggle('checked', seg.querySelector('input').checked));
    });
  });

  window.addRef = async function (event, url) {
    event.preventDefault();
    const input = event.target.querySelector('input[name="ref"]');
    const data = await call('POST', url, { url: input.value }, event.submitter);
    if (data && data.redirect) go(data.redirect);
    return false;
  };

  window.startRun = async function (url, kind, button, extra) {
    const body = Object.assign({ kind, runner: selectedRunner() }, extra || {});
    const data = await call('POST', url, body, button);
    if (data && data.redirect) go(data.redirect);
  };

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
    waiting.innerHTML = '<div class="msg-role"><span class="spinner"></span> агент думает…</div>';
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

  // Tooltips: one floating element positioned near the hovered control.
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
      const width = Math.min(380, window.innerWidth - 24);
      tooltip.style.maxWidth = width + 'px';
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

  // Elapsed timers for running boxes.
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

  // Poll active runs; reload the page when one finishes.
  const active = Array.from(document.querySelectorAll('[data-run-status]')).filter((el) => /queued|running/.test(el.dataset.status));
  if (active.length) {
    const ids = Array.from(new Set(active.map((el) => el.dataset.runStatus)));
    const logTail = document.getElementById('log-tail');
    const tick = async () => {
      let finished = false;
      for (const id of ids) {
        try {
          const response = await fetch(`/api/runs/${id}`);
          if (!response.ok) continue;
          const data = await response.json();
          if (!/queued|running/.test(data.status)) finished = true;
        } catch (error) { /* server restarting */ }
      }
      if (finished) { reload(); return; }
      if (logTail && ids.length === 1) {
        try {
          const text = await (await fetch(`/run/${ids[0]}/log`)).text();
          logTail.textContent = text.slice(-12000);
          logTail.scrollTop = logTail.scrollHeight;
        } catch (error) { /* ignore */ }
      }
      setTimeout(tick, 4000);
    };
    setTimeout(tick, 4000);
  }
})();
