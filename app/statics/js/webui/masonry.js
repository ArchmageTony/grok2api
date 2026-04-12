(() => {
  const VERIFY_ENDPOINT = '/webui/api/verify';
  const IMAGINE_WS_ENDPOINT = '/webui/api/imagine/ws';
  const IMAGE_COUNT = 6;
  const PROMPT_MIN_HEIGHT = 52;
  const PROMPT_MAX_HEIGHT = 160;
  const RECONNECT_DELAY_MS = 250;

  const promptInput = document.getElementById('promptInput');
  const sendBtn = document.getElementById('sendBtn');
  const feed = document.getElementById('masonryFeed');
  const emptyState = document.getElementById('masonryEmpty');
  const statusEl = document.getElementById('masonryStatus');
  const aspectRatioSelect = document.getElementById('aspectRatioSelect');
  const aspectRatioWrap = document.getElementById('aspectRatioWrap');
  const qualityToggle = document.getElementById('qualityToggle');
  const modeToggle = document.getElementById('modeToggle');

  let activeSocket = null;
  let sending = false;
  let streamState = null;

  function aspectRatioCss(value) {
    const ratio = String(value || '').trim();
    if (!ratio.includes(':')) return '1 / 1';
    const [w, h] = ratio.split(':');
    const width = Number(w) || 1;
    const height = Number(h) || 1;
    return `${width} / ${height}`;
  }

  function computeGridColumns(width) {
    const viewport = window.innerWidth || width || 0;
    const maxCols = viewport <= 768 ? 3 : 8;
    const minTileWidth = viewport <= 768 ? 132 : 188;
    const safeWidth = Math.max(width || 0, minTileWidth);
    return Math.max(1, Math.min(maxCols, Math.floor(safeWidth / minTileWidth)));
  }

  function syncBatchGrid(grid) {
    if (!(grid instanceof HTMLElement)) return;
    const columns = computeGridColumns(grid.clientWidth);
    grid.style.setProperty('--masonry-columns', String(columns));
  }

  function syncAllBatchGrids() {
    feed?.querySelectorAll('.webui-masonry-grid').forEach((grid) => {
      syncBatchGrid(grid);
    });
  }

  function text(key, fallback, params) {
    if (typeof window.t !== 'function') return fallback;
    const value = t(key, params);
    return value === key ? fallback : value;
  }

  function toast(message, type = 'info') {
    if (typeof showToast === 'function') showToast(message, type);
  }

  function buildWebSocketUrl(path, params = {}) {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const url = new URL(path, `${protocol}//${window.location.host}`);
    Object.entries(params).forEach(([key, value]) => {
      if (value === null || value === undefined || value === '') return;
      url.searchParams.set(key, String(value));
    });
    return url.toString();
  }

  function resizePromptInput() {
    if (!promptInput) return;
    promptInput.style.height = `${PROMPT_MIN_HEIGHT}px`;
    const nextHeight = Math.min(Math.max(promptInput.scrollHeight, PROMPT_MIN_HEIGHT), PROMPT_MAX_HEIGHT);
    promptInput.style.height = `${nextHeight}px`;
    promptInput.style.overflowY = promptInput.scrollHeight > PROMPT_MAX_HEIGHT ? 'auto' : 'hidden';
  }

  function setEmptyState() {
    if (!feed || !emptyState) return;
    const hasBatch = feed.querySelector('.webui-masonry-batch') !== null;
    emptyState.hidden = hasBatch;
    emptyState.style.display = hasBatch ? 'none' : '';
  }

  function setStatus(message, state = 'idle') {
    if (!statusEl) return;
    statusEl.textContent = message;
    statusEl.dataset.state = state;
  }

  function renderSendButton(running) {
    if (!sendBtn) return;
    const label = running
      ? text('webui.masonry.stop', '停止')
      : text('webui.masonry.generate', '生成');
    const icon = running
      ? '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M10 7v10"></path><path d="M14 7v10"></path></svg>'
      : '<svg viewBox="0 0 24 24" aria-hidden="true"><path d="M12 5v14"></path><path d="m6 11 6-6 6 6"></path></svg>';
    sendBtn.innerHTML = icon;
    sendBtn.setAttribute('aria-label', label);
    sendBtn.setAttribute('title', label);
  }

  function setSending(next) {
    sending = next;
    if (promptInput) promptInput.disabled = next;
    if (aspectRatioSelect) aspectRatioSelect.disabled = next;
    qualityToggle?.querySelectorAll('.webui-masonry-toggle-btn').forEach((button) => {
      button.disabled = next;
    });
    modeToggle?.querySelectorAll('.webui-masonry-toggle-btn').forEach((button) => {
      button.disabled = next;
    });
    renderSendButton(next);
  }

  function formatRoundLabel(round) {
    return text('webui.masonry.roundLabel', '第 {n} 轮', { n: round });
  }

  function formatQualityLabel(quality) {
    return quality === 'quality'
      ? text('webui.masonry.qualityQuality', 'Quality')
      : text('webui.masonry.qualitySpeed', 'Speed');
  }

  function syncAspectRatioIndicator() {
    const ratio = aspectRatioSelect?.value || '2:3';
    if (aspectRatioWrap) aspectRatioWrap.dataset.ratio = ratio;
  }

  function readToggleValue(group, fallback) {
    const active = group?.querySelector('.webui-masonry-toggle-btn.is-active');
    return active?.dataset.value || fallback;
  }

  function setToggleValue(group, value) {
    group?.querySelectorAll('.webui-masonry-toggle-btn').forEach((button) => {
      const active = button.dataset.value === value;
      button.classList.toggle('is-active', active);
      button.setAttribute('aria-pressed', active ? 'true' : 'false');
    });
  }

  function createSlot(index) {
    const tile = document.createElement('article');
    tile.className = 'webui-masonry-tile is-pending';

    const badge = document.createElement('div');
    badge.className = 'webui-masonry-tile-badge';
    badge.textContent = String(index);

    const body = document.createElement('div');
    body.className = 'webui-masonry-tile-body';
    tile.appendChild(badge);
    tile.appendChild(body);

    return {
      index,
      id: '',
      url: '',
      progress: 0,
      moderated: false,
      tile,
      body,
    };
  }

  function createBatch(prompt, aspectRatio, quality, round) {
    const wrap = document.createElement('section');
    wrap.className = 'webui-masonry-batch';

    const head = document.createElement('header');
    head.className = 'webui-masonry-batch-head';

    const promptEl = document.createElement('div');
    promptEl.className = 'webui-masonry-batch-prompt';
    promptEl.textContent = prompt;

    const meta = document.createElement('div');
    meta.className = 'webui-masonry-batch-meta';

    const ratio = document.createElement('span');
    ratio.className = 'webui-masonry-batch-chip is-param';
    ratio.textContent = aspectRatio;

    const qualityChip = document.createElement('span');
    qualityChip.className = 'webui-masonry-batch-chip is-param';
    qualityChip.textContent = formatQualityLabel(quality);

    const roundLabel = document.createElement('span');
    roundLabel.className = 'webui-masonry-batch-chip is-round';
    roundLabel.textContent = formatRoundLabel(round);

    const count = document.createElement('span');
    count.className = 'webui-masonry-batch-chip is-count';
    count.textContent = `0/${IMAGE_COUNT}`;

    const state = document.createElement('span');
    state.className = 'webui-masonry-batch-chip is-state';
    state.dataset.state = 'generating';
    state.textContent = text('webui.masonry.statusGenerating', '正在生成');

    meta.appendChild(roundLabel);
    meta.appendChild(ratio);
    meta.appendChild(qualityChip);
    meta.appendChild(count);
    meta.appendChild(state);

    head.appendChild(promptEl);
    head.appendChild(meta);

    const grid = document.createElement('div');
    grid.className = 'webui-masonry-grid';
    grid.style.setProperty('--tile-aspect', aspectRatioCss(aspectRatio));

    const slots = Array.from({ length: IMAGE_COUNT }, (_, index) => createSlot(index + 1));
    slots.forEach((slot) => {
      renderSlot(slot);
      grid.appendChild(slot.tile);
    });

    wrap.appendChild(head);
    wrap.appendChild(grid);
    feed.prepend(wrap);
    setEmptyState();
    syncBatchGrid(grid);

    return {
      wrap,
      count,
      state,
      slots,
      round,
      prompt,
      aspectRatio,
      quality,
      completed: false,
      failed: false,
      finalized: false,
    };
  }

  function updateBatchMeta(batch) {
    const completed = batch.slots.filter((slot) => slot.url && !slot.moderated).length;
    const filtered = batch.slots.filter((slot) => slot.moderated).length;
    batch.count.textContent = `${completed}/${IMAGE_COUNT}`;

    if (!batch.finalized) {
      batch.state.dataset.state = 'generating';
      batch.state.textContent = text('webui.masonry.statusGenerating', '正在生成');
      return;
    }

    if (completed >= IMAGE_COUNT) {
      batch.state.dataset.state = 'success';
      batch.state.textContent = text('webui.masonry.statusSuccess', '生成成功');
      return;
    }

    if (completed > 0 || (completed === 0 && filtered > 0 && filtered < IMAGE_COUNT)) {
      batch.state.dataset.state = 'partial';
      batch.state.textContent = text('webui.masonry.statusPartialFailure', '部分失败');
      return;
    }

    batch.state.dataset.state = 'failed';
    batch.state.textContent = text('webui.masonry.statusBatchFailed', '生成失败');
  }

  function markBatchFailed(batch) {
    if (!batch || batch.finalized) return;
    batch.failed = true;
    batch.finalized = true;
    updateBatchMeta(batch);
  }

  function markBatchStopped(batch) {
    if (!batch || batch.finalized) return;
    batch.finalized = true;
    updateBatchMeta(batch);
  }

  function renderSlot(slot) {
    slot.tile.classList.remove('is-pending', 'is-ready', 'is-filtered');
    slot.body.replaceChildren();

    if (slot.url) {
      slot.tile.classList.add('is-ready');
      const link = document.createElement('a');
      link.className = 'webui-masonry-tile-link';
      link.href = slot.url;
      link.target = '_blank';
      link.rel = 'noopener';
      link.setAttribute('aria-label', text('webui.masonry.openImage', '打开图片'));
      link.title = text('webui.masonry.openImage', '打开图片');

      const img = document.createElement('img');
      img.src = slot.url;
      img.alt = `image ${slot.index}`;
      img.loading = 'lazy';

      link.appendChild(img);
      slot.body.appendChild(link);
      return;
    }

    const label = document.createElement('div');
    if (slot.moderated) {
      label.className = 'webui-masonry-tile-label';
      label.textContent = text('webui.masonry.batchFiltered', '已过滤');
      slot.tile.classList.add('is-filtered');
    } else {
      const progress = Math.max(0, Math.min(99, slot.progress || 0));
      const shell = document.createElement('div');
      shell.className = 'webui-masonry-tile-progress';

      const value = document.createElement('div');
      value.className = 'webui-masonry-tile-progress-value';
      value.textContent = `${progress}%`;

      const track = document.createElement('div');
      track.className = 'webui-masonry-tile-progress-track';

      const fill = document.createElement('div');
      fill.className = 'webui-masonry-tile-progress-fill';
      fill.style.width = `${progress}%`;

      track.appendChild(fill);
      shell.appendChild(value);
      shell.appendChild(track);
      slot.tile.classList.add('is-pending');
      slot.body.appendChild(shell);
      return;
    }

    slot.body.appendChild(label);
  }

  function findSlot(batch, payload) {
    const imageId = String(payload.image_id || '').trim();
    const order = Number(payload.order);
    let slot = null;

    if (imageId) slot = batch.slots.find((item) => item.id === imageId) || null;
    if (!slot && Number.isInteger(order) && order >= 0 && order < batch.slots.length) {
      const orderedSlot = batch.slots[order] || null;
      if (orderedSlot && !orderedSlot.url && !orderedSlot.moderated) {
        slot = orderedSlot;
      }
    }
    if (!slot) {
      slot = batch.slots.find((item) => !item.url && !item.moderated) || null;
    }
    if (!slot) {
      slot = batch.slots.find((item) => !item.id && !item.url && !item.moderated) || batch.slots[0];
    }
    if (slot && imageId) slot.id = imageId;
    return slot;
  }

  function syncBatch(batch, payload) {
    const slot = findSlot(batch, payload);
    if (!slot) return;

    if (payload.type === 'progress') {
      slot.progress = Number(payload.progress) || slot.progress || 0;
    } else if (payload.type === 'image') {
      slot.progress = 100;
      slot.url = String(payload.url || '').trim();
      slot.moderated = false;
    } else if (payload.type === 'moderated') {
      slot.progress = 100;
      slot.url = '';
      slot.moderated = true;
    }

    renderSlot(slot);
    updateBatchMeta(batch);
  }

  function createStreamState(prompt, aspectRatio, quality, mode) {
    return {
      prompt,
      aspectRatio,
      quality,
      mode,
      keepRunning: true,
      userStopped: false,
      failed: false,
      batchIndex: 0,
      currentRunId: '',
      currentBatch: null,
      nextBatchQueued: false,
    };
  }

  async function ensureAccess() {
    const stored = await webuiKey.get();
    if (stored && await verifyKey(VERIFY_ENDPOINT, stored)) return true;
    if (stored) webuiKey.clear();
    if (await verifyKey(VERIFY_ENDPOINT, '')) return true;
    location.href = '/webui/login';
    return false;
  }

  async function connectSocket() {
    if (!streamState?.keepRunning) return;
    if (activeSocket && (activeSocket.readyState === WebSocket.OPEN || activeSocket.readyState === WebSocket.CONNECTING)) {
      return;
    }

    const token = await webuiKey.get();
    const wsUrl = buildWebSocketUrl(IMAGINE_WS_ENDPOINT, token ? { access_token: token } : {});
    const socket = new WebSocket(wsUrl);
    activeSocket = socket;

    socket.addEventListener('open', () => {
      if (activeSocket !== socket || !streamState?.keepRunning) return;
      scheduleNextBatch();
    });

    socket.addEventListener('message', (event) => {
      const state = streamState;
      if (!state) return;

      let payload;
      try {
        payload = JSON.parse(String(event.data || '{}'));
      } catch {
        return;
      }
      if (!payload || typeof payload !== 'object') return;

      if (payload.run_id && !state.currentRunId) state.currentRunId = String(payload.run_id);
      if (state.currentRunId && payload.run_id && String(payload.run_id) !== state.currentRunId) return;

      const batch = state.currentBatch;
      if (payload.type === 'status') {
        if (payload.status === 'running') {
          setStatus(`${text('webui.masonry.statusGenerating', '生成中…')} · ${formatRoundLabel(state.batchIndex)}`, 'running');
        } else if (payload.status === 'completed') {
          if (batch) {
            batch.completed = true;
            batch.finalized = true;
            updateBatchMeta(batch);
          }
          state.currentRunId = '';
          if (state.keepRunning && !state.userStopped && state.mode === 'continuous') {
            scheduleNextBatch();
          } else {
            state.keepRunning = false;
            setStatus(text('webui.masonry.statusCompleted', '完成'), 'completed');
            try {
              socket.close(1000, 'completed');
            } catch {}
          }
        } else if (payload.status === 'stopped') {
          state.userStopped = true;
          state.keepRunning = false;
          markBatchStopped(batch);
          setStatus(text('webui.masonry.statusStopped', '已停止'), 'stopped');
        }
        return;
      }

      if (!batch) return;

      if (payload.type === 'progress' || payload.type === 'image' || payload.type === 'moderated') {
        syncBatch(batch, payload);
        const ready = batch.slots.filter((slot) => slot.url && !slot.moderated).length;
        setStatus(`${text('webui.masonry.statusGenerating', '生成中…')} ${ready}/${IMAGE_COUNT} · ${formatRoundLabel(batch.round)}`, 'running');
        return;
      }

      if (payload.type === 'error') {
        state.failed = true;
        state.keepRunning = false;
        markBatchFailed(batch);
        setStatus(text('webui.masonry.statusFailed', '失败'), 'failed');
        toast(payload.message || text('webui.masonry.errors.requestFailed', '请求失败'), 'error');
        try {
          socket.close(1011, 'error');
        } catch {}
      }
    });

    socket.addEventListener('error', () => {
      if (streamState?.keepRunning && !streamState.userStopped) {
        setStatus(text('webui.masonry.statusConnecting', '连接中…'), 'connecting');
      }
    });

    socket.addEventListener('close', () => {
      const state = streamState;
      if (activeSocket === socket) activeSocket = null;
      if (!state) {
        setSending(false);
        return;
      }

      if (state.userStopped) {
        markBatchStopped(state.currentBatch);
        setSending(false);
        streamState = null;
        setStatus(text('webui.masonry.statusStopped', '已停止'), 'stopped');
        promptInput?.focus();
        return;
      }

      if (state.failed) {
        markBatchFailed(state.currentBatch);
        setSending(false);
        streamState = null;
        setStatus(text('webui.masonry.statusFailed', '失败'), 'failed');
        promptInput?.focus();
        return;
      }

      if (state.keepRunning) {
        if (state.currentBatch && !state.currentBatch.completed) {
          markBatchFailed(state.currentBatch);
        }
        setStatus(text('webui.masonry.statusConnecting', '连接中…'), 'connecting');
        window.setTimeout(() => {
          if (streamState === state && state.keepRunning) {
            void connectSocket();
          }
        }, RECONNECT_DELAY_MS);
        return;
      }

      setSending(false);
      streamState = null;
      promptInput?.focus();
    });
  }

  function scheduleNextBatch() {
    const state = streamState;
    if (!state?.keepRunning || state.userStopped || state.failed || state.nextBatchQueued) return;
    state.nextBatchQueued = true;

    window.setTimeout(() => {
      const current = streamState;
      if (current !== state) return;
      state.nextBatchQueued = false;
      if (!state.keepRunning || state.userStopped || state.failed) return;
      if (!activeSocket || activeSocket.readyState !== WebSocket.OPEN) {
        void connectSocket();
        return;
      }

      state.batchIndex += 1;
      state.currentRunId = '';
      state.currentBatch = createBatch(state.prompt, state.aspectRatio, state.quality, state.batchIndex);
      setStatus(`${text('webui.masonry.statusGenerating', '生成中…')} · ${formatRoundLabel(state.batchIndex)}`, 'running');

      try {
        activeSocket.send(JSON.stringify({
          type: 'start',
          prompt: state.prompt,
          aspect_ratio: state.aspectRatio,
          quality: state.quality,
          count: IMAGE_COUNT,
        }));
      } catch {
        void connectSocket();
      }
    }, 0);
  }

  function stopGeneration({ silent = false } = {}) {
    if (streamState) {
      streamState.keepRunning = false;
      streamState.userStopped = true;
      streamState.nextBatchQueued = false;
    }
    const socket = activeSocket;
    if (socket) socket.__userStopped = true;
    activeSocket = null;
    if (socket) {
      try {
        socket.send(JSON.stringify({ type: 'stop' }));
      } catch {}
      try {
        socket.close(1000, 'stopped');
      } catch {}
    }
    setSending(false);
    setStatus(text('webui.masonry.statusStopped', '已停止'), 'stopped');
    markBatchStopped(streamState?.currentBatch);
    streamState = null;
    if (!silent) toast(text('webui.masonry.statusStopped', '已停止'), 'info');
  }

  async function startGeneration() {
    if (sending) {
      stopGeneration({ silent: true });
      return;
    }

    const prompt = String(promptInput?.value || '').trim();
    if (!prompt) {
      toast(text('webui.masonry.errors.enterPrompt', '请输入提示词'), 'error');
      return;
    }

    const aspectRatio = aspectRatioSelect?.value || '2:3';
    const quality = readToggleValue(qualityToggle, 'speed') === 'quality' ? 'quality' : 'speed';
    const mode = readToggleValue(modeToggle, 'single') === 'continuous' ? 'continuous' : 'single';
    streamState = createStreamState(prompt, aspectRatio, quality, mode);
    setSending(true);
    setStatus(text('webui.masonry.statusConnecting', '连接中…'), 'connecting');
    promptInput.value = '';
    resizePromptInput();
    await connectSocket();
  }

  async function boot() {
    await renderWebuiHeader?.();
    await renderSiteFooter?.();
    window.I18n?.apply?.(document);
    if (!await ensureAccess()) return;
    syncAspectRatioIndicator();
    setToggleValue(qualityToggle, 'speed');
    setToggleValue(modeToggle, 'single');
    setSending(false);
    setEmptyState();
    setStatus(text('webui.masonry.statusReady', '就绪'), 'idle');
    resizePromptInput();
    promptInput?.focus();
  }

  sendBtn?.addEventListener('click', () => {
    void startGeneration();
  });

  qualityToggle?.addEventListener('click', (event) => {
    const button = event.target instanceof Element ? event.target.closest('.webui-masonry-toggle-btn') : null;
    if (!(button instanceof HTMLButtonElement) || button.disabled) return;
    setToggleValue(qualityToggle, button.dataset.value || 'speed');
  });

  modeToggle?.addEventListener('click', (event) => {
    const button = event.target instanceof Element ? event.target.closest('.webui-masonry-toggle-btn') : null;
    if (!(button instanceof HTMLButtonElement) || button.disabled) return;
    setToggleValue(modeToggle, button.dataset.value || 'single');
  });

  promptInput?.addEventListener('input', resizePromptInput);
  aspectRatioSelect?.addEventListener('change', syncAspectRatioIndicator);
  promptInput?.addEventListener('keydown', (event) => {
    if (event.key === 'Enter' && !event.shiftKey) {
      event.preventDefault();
      void startGeneration();
    }
  });

  window.addEventListener('beforeunload', () => {
    if (!activeSocket) return;
    try {
      activeSocket.close(1000, 'unload');
    } catch {}
    activeSocket = null;
  });
  window.addEventListener('resize', syncAllBatchGrids);

  boot().catch((error) => {
    console.error('webui masonry boot failed', error);
    toast(text('webui.masonry.errors.initFailed', '瀑布流页面初始化失败'), 'error');
    setStatus(text('webui.masonry.statusFailed', '失败'), 'failed');
  });
})();
