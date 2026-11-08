// RepoDeploy panel behaviour. Served from /static so the Content-Security-Policy
// can forbid inline scripts; every page works without it.
(() => {
  'use strict';

  // Confirmation prompts for consequential buttons: data-confirm="question".
  document.addEventListener('click', (event) => {
    const trigger = event.target.closest('[data-confirm]');
    if (trigger && !window.confirm(trigger.dataset.confirm)) {
      event.preventDefault();
    }
  });

  // Copy buttons: data-copy="id-of-field".
  document.querySelectorAll('[data-copy]').forEach((button) => {
    button.addEventListener('click', async () => {
      const source = document.getElementById(button.dataset.copy);
      if (!source) return;
      const text = 'value' in source ? source.value : source.textContent;
      try {
        await navigator.clipboard.writeText(text);
        button.textContent = 'Copied';
      } catch {
        if (source.select) source.select();
      }
    });
  });

  // Forms that continue to an external provider submit themselves.
  document.querySelectorAll('form[data-autosubmit]').forEach((form) => form.submit());

  // Show stored UTC timestamps in the viewer's local time.
  document.querySelectorAll('time[datetime]').forEach((element) => {
    const date = new Date(element.getAttribute('datetime'));
    if (!Number.isNaN(date.getTime())) {
      element.title = element.textContent;
      element.textContent = date.toLocaleString();
    }
  });

  // Matches the "phaseClass" template so live updates keep the same colours.
  const phaseClass = (phase) => {
    if (phase === 'succeeded') return 'ok';
    if (phase === 'failed') return 'failed';
    if (phase === 'queued' || phase === 'waiting_ci') return 'pending';
    return 'running';
  };

  // Live deployment logs: <pre data-log-stream="/api/jobs/ID/logs?follow=1">.
  const logs = document.querySelector('[data-log-stream]');
  if (logs) {
    const phase = document.getElementById('phase');
    const stream = new EventSource(logs.dataset.logStream);
    stream.onmessage = (event) => {
      const data = JSON.parse(event.data);
      logs.textContent += data.log;
      logs.scrollTop = logs.scrollHeight;
      if (phase && data.phase) {
        phase.textContent = data.phase;
        phase.className = 'status ' + phaseClass(data.phase);
      }
    };
    stream.addEventListener('done', () => {
      stream.close();
      // Reload once so the final error and recovery state are shown.
      if (!logs.dataset.final) window.setTimeout(() => window.location.reload(), 500);
    });
    stream.onerror = () => {
      stream.close();
      const note = document.createElement('p');
      note.className = 'notice';
      note.textContent = 'Log stream disconnected. Reload to reconnect.';
      (logs.closest('.terminal') || logs).after(note);
    };
  }

  // Refresh the dashboard while deployments run, unless the user is editing.
  if (document.body.dataset.refresh) {
    window.setTimeout(() => {
      const active = document.activeElement;
      const editing = document.querySelector('details[open]') || (active && ['INPUT', 'TEXTAREA', 'SELECT'].includes(active.tagName));
      if (!editing) window.location.reload();
    }, 10000);
  }
})();
