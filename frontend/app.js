(() => {
  'use strict';

  const app = document.getElementById('app');
  const roleLabel = document.getElementById('role-label');
  const statusLabel = document.getElementById('status-label');
  const statusDot = document.getElementById('status-dot');
  const announcement = document.getElementById('announcement');
  const operationErrorElement = document.getElementById('operation-error');
  const quitButton = document.getElementById('quit-button');
  const sections = ['connection', 'projects', 'resources', 'diagnostics'];
  let selectedSection = 'connection';
  let lastRevision = -1;
  let currentState = null;
  let localBusy = false;
  let pollTimer = 0;
  let snapshotInFlight = false;
  let stopped = false;
  let manualAddressRevealed = false;
  let operationError = '';

  const escapeHTML = (value) => String(value ?? '')
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;')
    .replaceAll("'", '&#39;');

  const bridge = () => window.go?.main?.UIBridge;

  function heading(id, eyebrow, title, detail) {
    return `<div class="page-heading"><div class="eyebrow">${escapeHTML(eyebrow)}</div><h1 id="${id}" tabindex="-1">${escapeHTML(title)}</h1><p class="lead">${escapeHTML(detail)}</p></div>`;
  }

  function operationButton(operation, value = '', extraClass = '') {
    if (!operation || operation.id === 'quit') return '';
    const pending = Boolean(operation.pending);
    const label = pending ? operation.pendingLabel : operation.label;
    const style = operation.destructive ? 'danger' : (operation.id.includes('connect') || operation.id.includes('enable') || operation.id === 'start-search' || operation.id === 'approve-pair' ? 'primary' : 'secondary');
    const disabled = localBusy || !operation.enabled;
    return `<button class="${style} ${extraClass} ${pending ? 'loading' : ''}" data-operation="${escapeHTML(operation.id)}" data-value="${escapeHTML(value)}" ${disabled ? 'disabled' : ''}>${pending ? '<i class="button-spinner" aria-hidden="true"></i>' : ''}${escapeHTML(label)}</button>`;
  }

  function renderConnection(state) {
    const connection = state.connection || {};
    const devices = state.devices || [];
    let body = heading('connection-title', state.role || 'Remote Docker', connection.headline || 'Remote Docker', connection.detail || 'Состояние загружается.');
    if (connection.notice) {
      const disconnected = connection.disconnectedBy ? `Отключение: ${connection.disconnectedBy}. ` : '';
      body += `<div class="notice ${connection.tone === 'red' ? 'error-notice' : ''}">⚠ ${escapeHTML(disconnected + connection.notice)}</div>`;
    }
    if (connection.pairCode) {
      body += `<div class="code-panel"><div class="device-copy"><strong>${escapeHTML(connection.peerName || 'Устройство')}</strong><span>${escapeHTML(state.role)}</span></div><div class="pair-code">${escapeHTML(connection.pairCode)}</div><div class="pair-note">Код должен полностью совпадать на Mac и Windows.</div></div>`;
    } else if (state.lifecycle === 'connecting') {
      body += '<div class="progress-card"><strong>Защищённое подключение</strong><div class="progress-line"><div class="progress-fill"></div></div><div class="steps"><div class="step done"><span>Устройства подтвердили друг друга</span><span>Готово</span></div><div class="step"><span>TLS-туннель, Docker и синхронизация</span><span>Подключение…</span></div></div></div>';
    } else if (state.lifecycle === 'connected') {
      body += `<div class="role-card"><div><strong>${escapeHTML(connection.peerName || 'Подключённое устройство')}</strong><span>Защищённое соединение</span></div><div class="role-badge">${escapeHTML(connection.latency || 'Локальная сеть')}</div></div>`;
      body += `<div class="metrics"><div class="metric"><span>Docker</span><strong>${escapeHTML(connection.docker || 'Готов')}</strong><small>Windows</small></div><div class="metric"><span>Синхронизация</span><strong>${escapeHTML(connection.sync || 'Готова')}</strong><small>зарегистрированные проекты</small></div></div>`;
    } else if (state.lifecycle === 'searching') {
      body += '<div class="searching"><i class="spinner" aria-hidden="true"></i>Поиск продолжается</div>';
    } else if (state.lifecycle === 'reconnecting') {
      const side = connection.disconnectedBy ? ` Причина обнаружена на стороне: ${escapeHTML(connection.disconnectedBy)}.` : '';
      body += `<div class="notice error-notice">Повторное подключение выполняется автоматически${connection.countdown ? ` · ${escapeHTML(connection.countdown)}` : ''}.${side}</div>`;
    }
    if (devices.length) {
      body += `<div class="device-list">${devices.map(renderDevice).join('')}</div>`;
    } else if (state.lifecycle === 'searching' || state.lifecycle === 'client_ready' || state.lifecycle === 'host_waiting') {
      body += '<div class="empty">Устройства пока не найдены.</div>';
    }
    const canUseManualAddress = state.platform === 'darwin' && (state.lifecycle === 'searching' || state.lifecycle === 'client_ready');
    if (!canUseManualAddress) manualAddressRevealed = false;
    if (canUseManualAddress && !manualAddressRevealed) {
      body += '<button class="text-action" id="discovery-help" type="button">Windows не находится?</button>';
    } else if (canUseManualAddress) {
      body += '<form class="manual-address" id="manual-address-form"><label class="sr-only" for="manual-address-input">Частный IP-адрес Windows</label><input id="manual-address-input" name="address" type="text" inputmode="decimal" autocomplete="off" placeholder="Частный IP-адрес Windows, например 192.168.1.20" required><button class="secondary" type="submit">Подключиться</button></form><p class="path">Адрес проверяется повторно приложением. Разрешены только частные адреса локальной сети.</p>';
    }
    const actions = (state.operations || []).filter((operation) => operation.id !== 'quit');
    if (actions.length) body += `<div class="main-actions">${actions.map((operation) => operationButton(operation)).join('')}</div>`;
    document.getElementById('connection-section').innerHTML = body;
  }

  function renderDevice(device) {
    return `<article class="device"><div class="device-main"><div class="device-symbol"><svg aria-hidden="true"><use href="assets/icons.svg#device"></use></svg></div><div class="device-copy"><strong>${escapeHTML(device.name)}</strong><span>${escapeHTML(device.role)} · ${escapeHTML(device.status)}</span></div></div><div class="row-actions">${(device.operations || []).map((operation) => operationButton(operation, device.id, operation.id === 'forget-device' ? 'ghost' : '')).join('')}</div></article>`;
  }

  function renderProjects(state) {
    const projects = state.projects || [];
    let body = heading('projects-title', 'Проекты', 'Папки для удалённого Docker', 'Исходники остаются на Mac и синхронизируются в управляемую Linux-среду на Windows.');
    if (projects.length) {
      body += `<div class="workspace-list">${projects.map((project) => `<article class="workspace"><div><strong>${escapeHTML(project.name)}</strong><div class="path">${escapeHTML(project.path)}</div><div class="path">${escapeHTML(project.error || project.lastSuccess || project.syncStatus || 'Ожидание синхронизации')}</div></div><div class="row-actions">${(project.operations || []).map((operation) => operationButton(operation, project.id)).join('')}</div></article>`).join('')}</div>`;
    } else {
      body += '<div class="empty">Зарегистрированных проектов пока нет.</div>';
    }
    if (state.platform === 'darwin') {
      body += '<div class="main-actions"><button class="primary" id="pick-workspace" type="button">＋ Добавить папку проекта</button></div>';
    }
    document.getElementById('projects-section').innerHTML = body;
  }

  function renderResources(state) {
    const cards = state.resources?.cards || [];
    let body = heading('resources-title', 'Нагрузка', 'Где расходуются ресурсы', 'Docker работает на Windows, а Mac отвечает только за управление и синхронизацию.');
    body += `<div class="metrics">${cards.map((card) => `<article class="metric ${card.available ? '' : 'unavailable'}"><span>${escapeHTML(card.title)}</span><strong>${escapeHTML(card.available ? card.value : 'Недоступно')}</strong><small>${escapeHTML(card.detail || card.subtitle)}</small></article>`).join('')}</div>`;
    if (state.resources?.updatedAt) body += `<p class="path">Обновлено: ${escapeHTML(state.resources.updatedAt)}</p>`;
    document.getElementById('resources-section').innerHTML = body;
  }

  function renderDiagnostics(state) {
    const checks = state.diagnostics || [];
    let body = heading('diagnostics-title', 'Безопасные проверки', 'Диагностика соединения', 'Понятные названия вместо внутренних кодов. Проверки не выводят ключи, команды и переменные окружения.');
    if (checks.length) {
      body += `<div class="check-list">${checks.map((check) => `<article class="check"><div><strong>${escapeHTML(check.label)}</strong><div class="path">${escapeHTML(check.detail)}</div></div><span class="check-status ${escapeHTML(check.status)}">${escapeHTML(check.statusLabel)}</span></article>`).join('')}</div>`;
    } else {
      body += '<div class="empty">Нажмите «Обновить проверки», чтобы получить актуальное состояние.</div>';
    }
    const diagnosticsOperation = (state.operations || []).find((operation) => operation.id === 'diagnostics') || {id: 'diagnostics', label: 'Обновить проверки', pendingLabel: 'Проверяем…', enabled: true};
    body += `<div class="main-actions">${operationButton(diagnosticsOperation)}</div>`;
    document.getElementById('diagnostics-section').innerHTML = body;
  }

  function render(state) {
    if (!state || Number(state.revision ?? 0) < lastRevision) return;
    lastRevision = Number(state.revision ?? lastRevision);
    currentState = state;
    app.setAttribute('aria-busy', state.lifecycle === 'stopping' ? 'true' : 'false');
    roleLabel.textContent = state.role || 'Remote Docker';
    statusLabel.textContent = state.connection?.status || 'Недоступно';
    statusDot.className = `status-dot ${state.connection?.tone || 'gray'}`;
    renderConnection(state);
    renderProjects(state);
    renderResources(state);
    renderDiagnostics(state);
    renderOperationError(operationError || state.error || '');
    bindDynamicActions();
    updateGlobalBusy();
  }

  function bindDynamicActions() {
    document.querySelectorAll('[data-operation]').forEach((button) => {
      if (button.dataset.bound === 'true') return;
      button.dataset.bound = 'true';
      button.addEventListener('click', () => perform(button.dataset.operation, button.dataset.value || '', button));
    });
    const picker = document.getElementById('pick-workspace');
    if (picker) picker.addEventListener('click', pickWorkspace, {once: true});
    const discoveryHelp = document.getElementById('discovery-help');
    if (discoveryHelp) discoveryHelp.addEventListener('click', () => {
      manualAddressRevealed = true;
      renderConnection(currentState);
      bindDynamicActions();
      document.getElementById('manual-address-input')?.focus();
    }, {once: true});
    const manualAddress = document.getElementById('manual-address-form');
    if (manualAddress) manualAddress.addEventListener('submit', (event) => {
      event.preventDefault();
      const input = document.getElementById('manual-address-input');
      perform('manual-address', input?.value || '', manualAddress.querySelector('button'));
    }, {once: true});
  }

  function pendingExists(state) {
    const all = [...(state?.operations || [])];
    (state?.devices || []).forEach((device) => all.push(...(device.operations || [])));
    (state?.projects || []).forEach((project) => all.push(...(project.operations || [])));
    return all.some((operation) => operation.pending);
  }

  function updateGlobalBusy() {
    const busy = localBusy || pendingExists(currentState);
    document.querySelectorAll('button').forEach((button) => {
      if (button.matches('[data-section]')) return;
      const operation = button === quitButton
        ? findOperation('quit', '')
        : (button.matches('[data-operation]') ? findOperation(button.dataset.operation, button.dataset.value || '') : null);
      const enabled = operation ? operation.enabled : currentState?.lifecycle !== 'stopping';
      button.disabled = busy || !enabled;
    });
  }

  function renderOperationError(message) {
    operationErrorElement.textContent = message;
    operationErrorElement.hidden = !message;
  }

  function setOperationError(message) {
    operationError = message;
    renderOperationError(message);
  }

  async function perform(id, value, button) {
    if (localBusy || !id) return;
    if ((id === 'forget-device' || id === 'remove-project') && !window.confirm(id === 'forget-device' ? 'Забыть это устройство? Для следующего подключения потребуется безопасное сопряжение.' : 'Удалить проект из синхронизации? Исходные файлы на Mac останутся.')) return;
    setOperationError('');
    localBusy = true;
    const original = button?.textContent?.trim() || '';
    if (button) {
      button.classList.add('loading');
      button.disabled = true;
      const operation = findOperation(id, value);
      button.innerHTML = `<i class="button-spinner" aria-hidden="true"></i>${escapeHTML(operation?.pendingLabel || 'Выполняем…')}`;
    }
    updateGlobalBusy();
    announce(button?.textContent?.trim() || 'Выполняется действие');
    let nextState = null;
    try {
      const api = bridge();
      if (!api?.Perform) throw new Error('Окно не подключено к фоновому приложению.');
      nextState = await api.Perform(id, value);
      announce('Действие завершено');
    } catch (error) {
      const message = errorMessage(error);
      announce(message);
      setOperationError(message);
      if (currentState) nextState = {...currentState, error: message};
    } finally {
      localBusy = false;
      if (nextState) render(nextState);
      else if (button && document.body.contains(button)) {
        button.classList.remove('loading');
        button.textContent = original;
        updateGlobalBusy();
      }
    }
  }

  function findOperation(id, value) {
    const all = [...(currentState?.operations || [])];
    (currentState?.devices || []).forEach((device) => {
      if (!value || device.id === value) all.push(...(device.operations || []));
    });
    (currentState?.projects || []).forEach((project) => {
      if (!value || project.id === value) all.push(...(project.operations || []));
    });
    return all.find((operation) => operation.id === id);
  }

  async function pickWorkspace() {
    if (localBusy) return;
    setOperationError('');
    localBusy = true;
    updateGlobalBusy();
    let path = '';
    try {
      const api = bridge();
      if (!api?.PickWorkspace) throw new Error('Выбор папки недоступен.');
      path = await api.PickWorkspace();
    } catch (error) {
      const message = errorMessage(error);
      announce(message);
      setOperationError(message);
    } finally {
      localBusy = false;
      updateGlobalBusy();
    }
    if (path) await perform('add-project', path, document.getElementById('pick-workspace'));
  }

  async function quitApplication() {
    if (localBusy) return;
    setOperationError('');
    localBusy = true;
    quitButton.classList.add('loading');
    quitButton.disabled = true;
    quitButton.innerHTML = '<i class="button-spinner" aria-hidden="true"></i><span>Завершаем…</span>';
    updateGlobalBusy();
    announce('Безопасно завершаем Remote Docker');
    try {
      const api = bridge();
      if (!api?.Quit) throw new Error('Завершение работы недоступно.');
      await api.Quit();
      stopped = true;
      stopPolling();
    } catch (error) {
      localBusy = false;
      quitButton.classList.remove('loading');
      quitButton.disabled = false;
      quitButton.innerHTML = '<svg class="nav-icon" aria-hidden="true"><use href="assets/icons.svg#quit"></use></svg><span>Завершить работу</span>';
      const message = errorMessage(error);
      announce(message);
      setOperationError(message);
      updateGlobalBusy();
    }
  }

  async function snapshot() {
    if (stopped || document.visibilityState !== 'visible' || localBusy) return;
    if (snapshotInFlight) return;
    snapshotInFlight = true;
    try {
      const api = bridge();
      if (!api?.Snapshot) throw new Error('Ожидание фонового приложения…');
      render(await api.Snapshot());
    } catch (error) {
      announce(errorMessage(error));
      if (!currentState) renderUnavailable(errorMessage(error));
    } finally {
      snapshotInFlight = false;
    }
  }

  function renderUnavailable(message) {
    roleLabel.textContent = 'Remote Docker';
    statusLabel.textContent = 'Недоступно';
    statusDot.className = 'status-dot gray';
    document.getElementById('connection-section').innerHTML = heading('connection-title', 'Подключение', 'Фоновое приложение недоступно', message);
  }

  function errorMessage(error) {
    return String(error?.message || error || 'Действие не завершено. Повторите попытку.');
  }

  function announce(message) {
    announcement.textContent = '';
    window.setTimeout(() => { announcement.textContent = message; }, 10);
  }

  function startPolling() {
    if (stopped || pollTimer || snapshotInFlight || document.visibilityState !== 'visible') return;
    schedulePolling(0);
  }

  function schedulePolling(delay) {
    if (stopped || pollTimer || document.visibilityState !== 'visible') return;
    pollTimer = window.setTimeout(async () => {
      pollTimer = 0;
      await snapshot();
      schedulePolling(1000);
    }, delay);
  }

  function stopPolling() {
    if (pollTimer) window.clearTimeout(pollTimer);
    pollTimer = 0;
  }

  document.querySelectorAll('[data-section]').forEach((button) => button.addEventListener('click', () => {
    selectedSection = button.dataset.section;
    document.querySelectorAll('[data-section]').forEach((item) => {
      const active = item === button;
      item.classList.toggle('active', active);
      if (active) item.setAttribute('aria-current', 'page'); else item.removeAttribute('aria-current');
    });
    sections.forEach((section) => document.getElementById(`${section}-section`).classList.toggle('section-hidden', section !== selectedSection));
    document.getElementById(`${selectedSection}-section`)?.querySelector('h1')?.focus?.({preventScroll: true});
  }));

  quitButton.addEventListener('click', quitApplication);
  document.addEventListener('visibilitychange', () => document.visibilityState === 'visible' ? startPolling() : stopPolling());
  window.addEventListener('beforeunload', () => { stopped = true; stopPolling(); });
  startPolling();
})();
