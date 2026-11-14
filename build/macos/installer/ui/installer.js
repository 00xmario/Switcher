// Install Switcher: the screens, their animations, and the bridge to the
// native installer. The page never installs anything itself: it asks the
// native side, which does the work and reports each step back.
(() => {
  'use strict';

  const $ = (sel, root = document) => root.querySelector(sel);
  const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];
  const reduceMotion = () => matchMedia('(prefers-reduced-motion: reduce)').matches;
  const params = new URLSearchParams(location.search);

  // Inline style attributes are off under the page's CSP, so custom
  // properties ride in data-vars and are set through the CSSOM.
  for (const el of $$('[data-vars]')) {
    for (const pair of el.dataset.vars.split(';')) {
      const [name, value] = pair.split(':');
      if (name && value) el.style.setProperty(name.trim(), value.trim());
    }
  }

  // ---------- The Switcher mark ----------
  const TRACK = 'M68 28H44A11 11 0 0 0 44 50H56A11 11 0 0 1 56 72H32';
  const RUN = 'M-36 44H-12A11 11 0 0 0 -12 22H-24A11 11 0 0 1 -24 0H0';
  let markSerial = 0;

  function markSVG({ tile = false, mono = false, off = false, animate = false } = {}) {
    const id = `m${++markSerial}`;
    const knobAt = off ? [32, 72] : [68, 28];
    const track = mono ? 'currentColor' : off ? '#56665d' : `url(#${id}-g)`;
    const knob = mono ? 'currentColor' : off ? '#9aa8a0' : '#f4fff8';
    const timing = 'dur=".9s" begin="indefinite" fill="freeze" calcMode="spline" keyTimes="0;1" keySplines=".45 0 .2 1"';
    return `<svg class="switcher-mark" viewBox="${tile ? '0 0 100 100' : '22 12 62 76'}" width="100%" height="100%" aria-hidden="true" focusable="false">
      <defs>
        <linearGradient id="${id}-g" gradientUnits="userSpaceOnUse" x1="30" y1="22" x2="62" y2="80"><stop offset="0" stop-color="#d4f76e"/><stop offset=".5" stop-color="#43dc8d"/><stop offset="1" stop-color="#10a89a"/></linearGradient>
        <linearGradient id="${id}-bg" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#22332b"/><stop offset="1" stop-color="#0a120e"/></linearGradient>
        <radialGradient id="${id}-sh" cx=".28" cy=".02" r="1"><stop offset="0" stop-color="#fff" stop-opacity=".14"/><stop offset=".55" stop-color="#fff" stop-opacity="0"/></radialGradient>
        <mask id="${id}-cut" maskUnits="userSpaceOnUse" x="0" y="0" width="100" height="100"><rect width="100" height="100" fill="#fff"/><circle cx="${knobAt[0]}" cy="${knobAt[1]}" r="13.4" fill="#000"/></mask>
      </defs>
      ${tile ? `<rect width="100" height="100" rx="23" fill="url(#${id}-bg)"/><rect width="100" height="100" rx="23" fill="url(#${id}-sh)"/><rect x="1" y="1" width="98" height="98" rx="22" fill="none" stroke="#fff" stroke-opacity=".1" stroke-width="2"/>` : ''}
      <g ${tile ? 'transform="translate(50 50) scale(1.05) translate(-52.9 -47.1)"' : ''}>
        <path class="sw-track" d="${TRACK}" stroke="${track}" mask="url(#${id}-cut)" pathLength="1" stroke-dasharray="1 1">
          ${animate ? `<animate attributeName="stroke-dashoffset" from="-1" to="0" ${timing}/>` : ''}
        </path>
        <g transform="translate(${knobAt[0]} ${knobAt[1]})"><circle class="sw-knob" r="11" fill="${knob}">
          ${animate ? `<animateMotion path="${RUN}" ${timing}/>` : ''}
        </circle></g>
      </g>
    </svg>`;
  }

  const markKinds = {
    hero: { tile: true, animate: true, sheen: true },
    newer: { tile: true },
    tile: { tile: true },
    host: { tile: true },
    done: { tile: true, animate: true },
    off: { tile: true, off: true },
    mini: {},
    mono: { mono: true },
  };
  for (const el of $$('[data-mark]')) {
    const kind = markKinds[el.dataset.mark] || {};
    el.innerHTML = markSVG(kind) + (kind.sheen ? '<span class="sheen"></span>' : '');
    if (kind.animate) el.classList.add('unplayed');
  }

  function playMarks(root) {
    for (const el of $$('.unplayed', root)) el.classList.remove('unplayed');
    if (reduceMotion()) return;
    for (const a of $$('.switcher-mark animate, .switcher-mark animateMotion', root)) {
      try { a.beginElement(); } catch { /* SMIL missing: the mark stays whole */ }
    }
  }


  // ---------- Provider logos circling the mark ----------
  // Two tilted orbits; a chip passes behind the icon on the far side and in
  // front of it on the near side, growing as it comes closer.
  let orbitStarted = false;
  function startOrbit() {
    if (orbitStarted) return;
    orbitStarted = true;
    const orbits = [{ rx: 215, ry: 64, speed: 1 / 26000, phase: 0 }, { rx: 152, ry: 45, speed: -1 / 19000, phase: Math.PI / 3 }];
    const chips = $$('.orbit .chip').map((el, i) => {
      const o = orbits[Number(el.dataset.orbit)];
      const k = $$(`.orbit .chip[data-orbit="${el.dataset.orbit}"]`).indexOf(el);
      return { el, o, base: o.phase + k * (Math.PI * 2 / 3), delay: 950 + i * 110 };
    });
    const t0 = performance.now();
    const place = (t) => {
      for (const c of chips) {
        const grow = Math.min(1, Math.max(0, (t - t0 - c.delay) / 900));
        const ease = 1 - Math.pow(1 - grow, 3);
        const a = c.base + (reduceMotion() ? 0 : (t - t0) * c.o.speed * Math.PI * 2);
        const x = Math.cos(a) * c.o.rx * ease, y = Math.sin(a) * c.o.ry * ease;
        const depth = Math.sin(a);
        const scale = (.78 + .3 * (depth + 1) / 2) * (.4 + .6 * ease);
        c.el.style.transform = `translate(${x.toFixed(2)}px, ${y.toFixed(2)}px) scale(${scale.toFixed(3)})`;
        c.el.style.opacity = (grow * (.55 + .45 * (depth + 1) / 2)).toFixed(3);
        c.el.style.zIndex = depth > 0 ? 4 : 1;
      }
    };
    if (reduceMotion()) { place(t0 + 5000); return; }
    const tick = (t) => { if (current?.dataset.screen === 'welcome' || t - t0 < 2500) place(t); requestAnimationFrame(tick); };
    requestAnimationFrame(tick);
  }

  // ---------- Sparks drifting through the backdrop ----------
  function startSparks() {
    const canvas = $('.sparks');
    const ctx = canvas.getContext('2d');
    let w = 0, h = 0, dpr = 1;
    const sparks = [];
    const resize = () => {
      dpr = Math.min(devicePixelRatio || 1, 2);
      w = canvas.clientWidth; h = canvas.clientHeight;
      canvas.width = w * dpr; canvas.height = h * dpr;
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    };
    resize();
    addEventListener('resize', resize);
    for (let i = 0; i < 48; i++) {
      sparks.push({ x: Math.random() * w, y: Math.random() * h, r: .5 + Math.random() * 1.4, v: .08 + Math.random() * .28, p: Math.random() * Math.PI * 2, hue: Math.random() });
    }
    const draw = (t) => {
      ctx.clearRect(0, 0, w, h);
      for (const s of sparks) {
        const a = .15 + .45 * (0.5 + 0.5 * Math.sin(t / 900 + s.p));
        ctx.beginPath();
        ctx.fillStyle = s.hue < .5 ? `rgba(212,247,110,${a})` : `rgba(120,240,190,${a})`;
        ctx.arc(s.x, s.y, s.r, 0, Math.PI * 2);
        ctx.fill();
      }
    };
    if (reduceMotion()) { draw(0); return; }
    let last = performance.now();
    const tick = (t) => {
      const dt = Math.min(64, t - last); last = t;
      for (const s of sparks) {
        s.y -= s.v * dt / 16;
        s.x += Math.sin(t / 2400 + s.p) * .12;
        if (s.y < -4) { s.y = h + 4; s.x = Math.random() * w; }
      }
      draw(t);
      requestAnimationFrame(tick);
    };
    requestAnimationFrame(tick);
  }

  // ---------- Confetti for the finish ----------
  function confetti() {
    if (reduceMotion()) return;
    const canvas = $('.confetti');
    const ctx = canvas.getContext('2d');
    const dpr = Math.min(devicePixelRatio || 1, 2);
    const w = canvas.clientWidth, h = canvas.clientHeight;
    canvas.width = w * dpr; canvas.height = h * dpr;
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    const colors = ['#d4f76e', '#43dc8d', '#10a89a', '#f4fff8', '#9ff0c4'];
    const bits = [];
    for (let i = 0; i < 170; i++) {
      const angle = -Math.PI / 2 + (Math.random() - .5) * Math.PI * 1.25;
      const speed = 6 + Math.random() * 9;
      bits.push({
        x: w / 2 + (Math.random() - .5) * 60, y: h * .36,
        vx: Math.cos(angle) * speed, vy: Math.sin(angle) * speed,
        rot: Math.random() * Math.PI, vr: (Math.random() - .5) * .3,
        size: 4 + Math.random() * 6, color: colors[i % colors.length],
        shape: Math.random() < .3 ? 'dot' : 'bar', life: 0,
      });
    }
    const start = performance.now();
    const tick = (t) => {
      const age = t - start;
      ctx.clearRect(0, 0, w, h);
      for (const b of bits) {
        b.vx *= .985; b.vy = b.vy * .985 + .32;
        b.x += b.vx; b.y += b.vy; b.rot += b.vr;
        const alpha = Math.max(0, 1 - age / 3600);
        ctx.save();
        ctx.globalAlpha = alpha;
        ctx.translate(b.x, b.y); ctx.rotate(b.rot);
        ctx.fillStyle = b.color;
        if (b.shape === 'dot') { ctx.beginPath(); ctx.arc(0, 0, b.size / 2.4, 0, Math.PI * 2); ctx.fill(); }
        else ctx.fillRect(-b.size / 2, -b.size / 5, b.size, b.size / 2.5);
        ctx.restore();
      }
      if (age < 3700) requestAnimationFrame(tick); else ctx.clearRect(0, 0, w, h);
    };
    requestAnimationFrame(tick);
  }

  // ---------- Screens ----------
  let current = null;
  function show(name) {
    const next = $(`[data-screen="${name}"]`);
    if (!next || next === current) return;
    if (current) {
      const leaving = current;
      leaving.classList.add('is-leaving');
      leaving.classList.remove('is-active');
      setTimeout(() => leaving.classList.remove('is-leaving'), 700);
    }
    current = next;
    next.classList.add('is-active');
    document.body.dataset.current = name;
    onShow[name]?.();
    // Move focus into the new screen for keyboard and VoiceOver users.
    next.tabIndex = -1;
    setTimeout(() => next.focus({ preventScroll: true }), 450);
  }
  const primaryButton = () => current && $$('.primary', current).find((b) => !b.hidden && !b.disabled && b.offsetParent);

  // ---------- The bridge ----------
  // window.webkit.messageHandlers.installer exists in the installer app;
  // in a browser a simulated installer stands in, for design work.
  const native = window.webkit?.messageHandlers?.installer;
  const send = (msg) => native ? native.postMessage(msg) : mock(msg);
  window.installer = { receive: (event) => receive(event) };

  let env = null;
  // Install events are numbered: one the page already has, replayed after
  // a reload, is not shown twice.
  let lastSeq = 0;
  function receive(event) {
    if (event?.type === 'env') return setEnv(event.env);
    if (!['plan', 'step', 'done', 'error'].includes(event?.type)) return;
    if (event.seq) {
      if (event.seq <= lastSeq) return;
      lastSeq = event.seq;
    }
    enqueue(event);
  }

  // The install reports steps as fast as it does them; the list shows each
  // one for at least a moment, so it reads as it goes. The work never waits.
  const MIN_STEP = 520;
  const queue = [];
  const shownAt = {};
  let pumping = false;
  function enqueue(event) {
    queue.push(event);
    if (!pumping) pump();
  }
  function pump() {
    if (!queue.length) { pumping = false; return; }
    pumping = true;
    const e = queue[0];
    let wait = 0;
    if (e.type === 'step' && e.state !== 'active' && shownAt[e.id]) wait = MIN_STEP - (performance.now() - shownAt[e.id]);
    setTimeout(() => {
      queue.shift();
      if (e.type === 'plan') setPlan(e.steps);
      else if (e.type === 'step') { if (e.state === 'active') shownAt[e.id] = performance.now(); setStep(e); }
      else if (e.type === 'done') finished(e.result);
      else if (e.type === 'error') failed(e);
      pump();
    }, reduceMotion() ? 0 : Math.max(0, wait));
  }

  // ---------- Welcome ----------
  const onShow = {
    welcome() { playMarks($('[data-screen="welcome"]')); startOrbit(); },
    tour() { startTour(); },
    setup() { stopTour(); },
    done() { playMarks($('[data-screen="done"]')); confetti(); },
  };

  function setEnv(e) {
    env = e;
    document.body.classList.remove('is-loading');
    const relation = e.relation || 'fresh';
    document.body.dataset.relation = relation;
    const from = e.existing?.version;
    if (relation === 'newer') {
      $('[data-newer-sub]').textContent = `This Mac has Switcher ${from}; this installer has ${e.version}.`;
      show('newer');
      return;
    }
    const eyebrow = $('[data-welcome-eyebrow]');
    const go = $('[data-welcome-go]');
    const fine = $('[data-welcome-fine]');
    fine.textContent = `Switcher ${e.version} · Free and open source · macOS 13 or later`;
    if (relation === 'update') {
      eyebrow.textContent = `Update from ${from}`;
      go.innerHTML = `Update to ${escapeHTML(e.version)}<span class="arrow" aria-hidden="true">→</span>`;
      go.dataset.go = 'setup';
      addTourLink();
    } else if (relation === 'same') {
      eyebrow.textContent = `Switcher ${e.version} is installed`;
      go.innerHTML = 'Continue<span class="arrow" aria-hidden="true">→</span>';
      go.dataset.go = 'setup';
      addTourLink();
    }

    const title = $('[data-setup-title]');
    const sub = $('[data-setup-sub]');
    const install = $('[data-install]');
    $('[data-dest-name]').textContent = e.destinationName || 'Applications';
    $('input[name="login"]').checked = e.login !== false;
    if (relation === 'update') {
      title.textContent = 'Ready to update Switcher';
      sub.textContent = `${from} → ${e.version}. Switcher restarts by itself, and your accounts and settings stay as they are.`;
      install.textContent = 'Update Switcher';
      $('[data-option="skill"]').hidden = true;
    } else if (relation === 'same') {
      title.textContent = `Switcher ${e.version} is already installed`;
      sub.textContent = 'Install it again to repair it. Your accounts and settings stay as they are.';
      install.textContent = 'Reinstall';
      $('[data-option="skill"]').hidden = true;
      $('.screen-setup [data-open-app]').hidden = false;
    } else {
      sub.textContent = `Switcher ${e.version} goes into ${e.destinationName || 'Applications'} and lives in your menu bar.`;
    }
    $('[data-install-title]').textContent = relation === 'fresh' ? 'Installing Switcher' : relation === 'update' ? 'Updating Switcher' : 'Reinstalling Switcher';

    const start = params.get('screen');
    show(start && $(`[data-screen="${start}"]`) ? start : 'welcome');
    if (start === 'tour' && params.get('slide')) goSlide(Number(params.get('slide')) || 0);
    if (start === 'install') demoInstall();
    if (start === 'done') finished(demoResult());
    if (start === 'error') failed(demoError());
    if (params.get('sheet')) openSheet();
  }

  function addTourLink() {
    const actions = $('[data-screen="welcome"] .actions');
    if ($('[data-tour-link]', actions)) return;
    const b = document.createElement('button');
    b.className = 'ghost big';
    b.dataset.go = 'tour';
    b.dataset.tourLink = '';
    b.textContent = 'Take the tour';
    actions.prepend(b);
  }

  // ---------- Tour ----------
  const slides = $$('.slide');
  const segments = $('.segments');
  let slideIndex = 0;
  slides.forEach((slide, i) => {
    const seg = document.createElement('button');
    seg.className = 'segment';
    seg.setAttribute('role', 'tab');
    seg.setAttribute('aria-label', slide.querySelector('h2').textContent);
    seg.innerHTML = '<i></i>';
    seg.addEventListener('click', () => goSlide(i));
    seg.addEventListener('animationend', () => { if (i === slideIndex && i < slides.length - 1) goSlide(i + 1); });
    segments.append(seg);
  });
  const segs = $$('.segment');
  const tourSlides = $('.tour-slides');
  tourSlides.addEventListener('mouseenter', () => $('.screen-tour').classList.add('tour-paused'));
  tourSlides.addEventListener('mouseleave', () => $('.screen-tour').classList.remove('tour-paused'));

  function goSlide(i) {
    slideIndex = Math.max(0, Math.min(slides.length - 1, i));
    slides.forEach((s, j) => {
      s.classList.toggle('is-current', j === slideIndex);
      s.classList.toggle('is-past', j < slideIndex);
      s.setAttribute('aria-hidden', j === slideIndex ? 'false' : 'true');
    });
    segs.forEach((seg, j) => {
      seg.classList.remove('running');
      seg.classList.toggle('done', j < slideIndex);
      seg.setAttribute('aria-selected', j === slideIndex ? 'true' : 'false');
    });
    // Restart the running segment's fill from zero.
    void segs[slideIndex].offsetWidth;
    if (!reduceMotion()) segs[slideIndex].classList.add('running');
    const next = $('[data-next]');
    const last = slideIndex === slides.length - 1;
    next.innerHTML = `${last ? 'Continue' : 'Next'}<span class="arrow" aria-hidden="true">→</span>`;
    onSlide[slides[slideIndex].dataset.slide]?.(slides[slideIndex]);
  }
  function startTour() { goSlide(0); }
  function stopTour() { segs.forEach((s) => s.classList.remove('running')); }

  const onSlide = {
    limits(slide) {
      // The percentage follows the bar as it drains to the limit.
      const count = $('.count', slide);
      const t0 = performance.now();
      const tick = (t) => {
        if (!slide.classList.contains('is-current')) return;
        const phase = ((t - t0) % 7000) / 7000;
        const k = Math.min(1, Math.max(0, (phase - .08) / .32));
        const eased = 1 - Math.pow(1 - k, 3);
        count.textContent = `${Math.round(34 - 24 * eased)}% left`;
        requestAnimationFrame(tick);
      };
      if (reduceMotion()) count.textContent = '10% left'; else requestAnimationFrame(tick);
    },
    share(slide) { drawLinks(slide); },
    quota(slide) {
      for (const g of $$('.gauge', slide)) {
        const fill = $('.g-fill', g);
        const v = parseFloat(getComputedStyle(g).getPropertyValue('--v')) || 0;
        fill.style.transition = 'none';
        fill.style.strokeDasharray = '0 100';
        void fill.getBoundingClientRect();
        fill.style.transition = '';
        requestAnimationFrame(() => { fill.style.strokeDasharray = `${v * 100} 100`; });
        countUp($('b', g), Number($('b', g).dataset.count));
      }
    },
  };

  // The lines from the host to each device follow the nodes' real places.
  function drawLinks(slide) {
    const art = $('.art-share', slide);
    const svg = $('.links', art);
    const box = art.getBoundingClientRect();
    if (!box.width) return;
    svg.setAttribute('viewBox', `0 0 ${box.width} ${box.height}`);
    const center = (el) => { const r = el.getBoundingClientRect(); return [r.left - box.left + r.width / 2, r.top - box.top + r.height / 2]; };
    const [hx, hy] = center($('.node.host > span', art));
    [['l1', '.n1 svg'], ['l2', '.n2 svg'], ['l3', '.n3 svg']].forEach(([id, sel]) => {
      const [x, y] = center($(sel, art));
      const midY = (hy + y) / 2;
      $(`#${id}`, svg).setAttribute('d', id === 'l3' ? `M${hx} ${hy}L${x} ${y}` : `M${hx} ${hy}C${hx} ${midY} ${x} ${midY + 20} ${x} ${y}`);
    });
  }
  function countUp(el, to) {
    if (reduceMotion()) { el.textContent = to; return; }
    const start = performance.now();
    const step = (t) => {
      const k = Math.min(1, (t - start) / 1500);
      el.textContent = Math.round(to * (1 - Math.pow(1 - k, 3)));
      if (k < 1) requestAnimationFrame(step);
    };
    requestAnimationFrame(step);
  }

  // ---------- Setup ----------
  const options = () => ({
    login: $('input[name="login"]').checked,
    cli: $('input[name="cli"]').checked,
    skill: !$('[data-option="skill"]').hidden && $('input[name="skill"]').checked,
  });

  let starting = false;
  function startInstall() {
    if (starting) return;
    starting = true;
    const setup = $('[data-screen="setup"]');
    // Nothing on the screen answers while the app flies into the folder:
    // the install is already decided.
    setup.inert = true;
    setup.classList.add('is-flying');
    const chosen = options();
    setTimeout(() => {
      show('install');
      setup.classList.remove('is-flying');
      setup.inert = false;
      starting = false;
      resetProgress();
      lastSeq = 0;
      send({ type: 'install', options: chosen });
    }, reduceMotion() ? 0 : 900);
  }

  // ---------- Installing ----------
  const STEP_TEXT = {
    copy: ['Copy Switcher', 'Copying Switcher…'],
    check: ['Check that it is intact', 'Checking its signature and giving it a test run…'],
    place: ['Move it into place', 'Moving Switcher into place…'],
    stop: ['Close the Switcher that was running', 'Closing the Switcher that was running…'],
    cli: ['Add the switcher command', 'Adding the switcher command…'],
    skill: ['Teach your AI agents', 'Adding the Switcher skill…'],
    login: ['Open at login', 'Setting Switcher to open at login…'],
    launch: ['Start Switcher', 'Starting Switcher…'],
  };
  let plan = [];
  const stepState = {};
  function setPlan(ids) {
    // An install the native side starts by itself still shows its progress.
    if (current?.dataset.screen !== 'install') { show('install'); resetProgress(); }
    plan = ids.filter((id) => STEP_TEXT[id]);
    const list = $('[data-steps]');
    list.innerHTML = '';
    for (const id of plan) {
      stepState[id] = 'pending';
      const li = document.createElement('li');
      li.className = 'step';
      li.dataset.step = id;
      li.innerHTML = '<span class="state" aria-hidden="true"></span><span class="label"></span><span class="detail"></span>';
      $('.label', li).textContent = id === 'place' && env?.destinationName ? `Move it into ${env.destinationName}` : STEP_TEXT[id][0];
      list.append(li);
    }
    updateProgressTarget();
  }
  function setStep({ id, state, detail }) {
    const li = $(`[data-step="${id}"]`);
    if (!li) return;
    stepState[id] = state;
    li.className = `step ${state}`;
    $('.detail', li).textContent = state === 'done' || state === 'skipped' ? (detail || '') : '';
    if (state === 'active') $('[data-install-detail]').textContent = detail || STEP_TEXT[id][1];
    updateProgressTarget();
  }

  // The knob rides the S from its bottom end to "on" as the steps finish.
  const pm = $('.progress-mark');
  const pmFill = $$('.pm-fill', pm);
  const pmKnob = $('.pm-knob', pm);
  const pmHole = $('.pm-hole', pm);
  const pmTrack = $('.pm-track', pm);
  let pmLength = 0, shown = 0, target = 0, pmRunning = false;
  function resetProgress() {
    shown = 0; target = 0;
    pm.classList.remove('is-complete');
    drawProgress(0);
  }
  function updateProgressTarget() {
    if (!plan.length) return;
    let sum = 0;
    for (const id of plan) {
      const s = stepState[id];
      sum += s === 'done' || s === 'skipped' ? 1 : s === 'active' ? .45 : 0;
    }
    target = Math.min(1, sum / plan.length);
    if (!pmRunning) { pmRunning = true; requestAnimationFrame(animateProgress); }
  }
  function animateProgress() {
    shown += (target - shown) * (reduceMotion() ? 1 : .08);
    if (Math.abs(target - shown) < .001) shown = target;
    drawProgress(shown);
    if (shown !== target) requestAnimationFrame(animateProgress);
    else pmRunning = false;
  }
  function drawProgress(p) {
    if (!pmLength) pmLength = pmTrack.getTotalLength();
    const L = pmLength;
    for (const f of pmFill) {
      f.style.strokeDasharray = `${L * p} ${L + 2}`;
      f.style.strokeDashoffset = `${-L * (1 - p)}`;
    }
    const pt = pmTrack.getPointAtLength(L * (1 - p));
    pmKnob.setAttribute('cx', pt.x); pmKnob.setAttribute('cy', pt.y);
    pmHole.setAttribute('cx', pt.x); pmHole.setAttribute('cy', pt.y);
    $('[data-percent]').textContent = Math.round(p * 100);
  }

  // ---------- Done ----------
  function finished(result) {
    updateProgressTargetTo(1);
    pm.classList.add('is-complete');
    $('[data-install-detail]').textContent = 'All done.';
    const relation = env?.relation || 'fresh';
    const running = result?.running !== false;
    const version = result?.version || env?.version || '';
    const screen = $('[data-screen="done"]');
    screen.classList.toggle('not-running', !running);
    const primary = $('[data-finish]');
    const secondary = $('[data-open-dashboard]');
    if (!running) {
      $('[data-done-title]').textContent = 'Switcher is installed';
      $('[data-done-sub]').textContent = `Switcher ${version} is in ${env?.destinationName || 'Applications'}. Open it from there if it does not appear in your menu bar.`;
      primary.textContent = 'Open Switcher';
      primary.dataset.action = 'app';
      secondary.textContent = 'Done';
      secondary.dataset.action = 'finish';
    } else if (relation === 'fresh') {
      $('[data-done-title]').textContent = 'Switcher is ready';
      $('[data-done-sub]').innerHTML = 'It lives in your menu bar. Add your Claude, Codex and other accounts in its dashboard: click the <b>S</b>, then <b>Open web app</b>, or start here.';
      primary.textContent = 'Add your accounts';
      primary.dataset.action = 'dashboard-finish';
      secondary.textContent = 'Done';
      secondary.dataset.action = 'finish';
    } else {
      $('[data-done-title]').textContent = relation === 'update' ? 'Switcher is up to date' : 'Switcher is reinstalled';
      $('[data-done-sub]').textContent = `Switcher ${version} is running in your menu bar. Your accounts and settings stayed as they were.`;
      primary.textContent = 'Done';
      primary.dataset.action = 'finish';
      secondary.textContent = 'Open dashboard';
      secondary.dataset.action = 'dashboard';
    }
    const notes = $('[data-done-notes]');
    notes.innerHTML = '';
    const add = (html, warn) => {
      const li = document.createElement('li');
      if (warn) li.className = 'warn';
      li.innerHTML = html;
      li.style.animationDelay = `${.9 + notes.children.length * .08}s`;
      notes.append(li);
      return li;
    };
    if (result?.path) add(`In <code>${escapeHTML(result.path)}</code>`);
    if (result?.cli?.path) {
      if (result.cli.onPath) add('<code>switcher</code> in Terminal');
      else {
        const li = add(`Add <code>${escapeHTML(result.cli.dir)}</code> to your PATH <button type="button">Copy</button>`, true);
        $('button', li).addEventListener('click', (ev) => { send({ type: 'copy', text: result.cli.hint }); ev.target.textContent = 'Copied'; });
      }
    }
    if (result?.login) add('Opens at login');
    if (result?.skill) add('Your agents know Switcher');
    for (const w of result?.warnings || []) add(escapeHTML(w), true);
    setTimeout(() => show('done'), reduceMotion() ? 200 : 1100);
  }
  function updateProgressTargetTo(v) { target = v; if (!pmRunning) { pmRunning = true; requestAnimationFrame(animateProgress); } }

  // ---------- Something went wrong ----------
  function failed(e) {
    $('[data-error-title]').textContent = e.title || 'Switcher was not installed';
    $('[data-error-message]').textContent = e.message || 'Something went wrong.';
    const calm = e.calm || (e.changed ? '' : 'Nothing on your Mac was changed.');
    $('[data-error-calm]').textContent = calm;
    $('[data-error-calm]').hidden = !calm;
    $('[data-error-detail]').textContent = e.detail || e.message || '';
    $('.error-detail').hidden = !e.detail;
    setTimeout(() => show('error'), reduceMotion() ? 0 : 500);
  }

  // ---------- Why macOS asked ----------
  const sheet = $('[data-sheet]');
  function openSheet() { sheet.hidden = false; setTimeout(() => $('[data-close-sheet]').focus(), 50); }
  function closeSheet() { sheet.hidden = true; $('[data-why]').focus(); }

  // ---------- Clicks and keys ----------
  document.addEventListener('click', (ev) => {
    const b = ev.target.closest('button');
    if (!b) return;
    if (b.dataset.go) return show(b.dataset.go);
    if (b.matches('[data-next]')) return slideIndex < slides.length - 1 ? goSlide(slideIndex + 1) : show('setup');
    if (b.matches('[data-install]')) return startInstall();
    if (b.matches('[data-why]')) return openSheet();
    if (b.matches('[data-close-sheet]')) return closeSheet();
    if (b.matches('[data-open-repo]')) return send({ type: 'open', target: 'repo' });
    if (b.dataset.action) return doneAction(b.dataset.action);
    if (b.matches('[data-open-dashboard]')) return send({ type: 'open', target: 'dashboard' });
    if (b.matches('[data-open-app]')) return send({ type: 'open', target: 'app' });
    if (b.matches('[data-finish]')) return send({ type: 'finish' });
    if (b.matches('[data-quit]')) return send({ type: 'quit' });
    if (b.matches('[data-retry]')) return show('setup');
    if (b.matches('[data-copy-detail]')) { send({ type: 'copy', text: $('[data-error-detail]').textContent }); b.textContent = 'Copied'; }
  });
  function doneAction(action) {
    if (action === 'app') return send({ type: 'open', target: 'app' });
    if (action === 'dashboard') return send({ type: 'open', target: 'dashboard' });
    if (action === 'dashboard-finish') { send({ type: 'open', target: 'dashboard' }); return send({ type: 'finish' }); }
    if (action === 'finish') return send({ type: 'finish' });
  }
  sheet.addEventListener('click', (ev) => { if (ev.target === sheet) closeSheet(); });
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape' && !sheet.hidden) return closeSheet();
    if (!sheet.hidden) return;
    if (current?.dataset.screen === 'tour') {
      if (ev.key === 'ArrowRight') goSlide(slideIndex + 1);
      if (ev.key === 'ArrowLeft') goSlide(slideIndex - 1);
    }
    if (ev.key === 'Enter' && !(ev.target instanceof HTMLButtonElement) && !(ev.target instanceof HTMLInputElement)) primaryButton()?.click();
  });
  document.addEventListener('contextmenu', (ev) => ev.preventDefault());
  document.addEventListener('dragstart', (ev) => ev.preventDefault());

  function escapeHTML(s) {
    return String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  }

  // ---------- A simulated installer, for a browser ----------
  function mockEnv() {
    const existing = params.get('existing');
    const v = params.get('version') || '1.33.0';
    const cmp = (a, b) => { const x = a.split('.').map(Number), y = b.split('.').map(Number); for (let i = 0; i < 3; i++) if ((x[i] || 0) !== (y[i] || 0)) return (x[i] || 0) - (y[i] || 0); return 0; };
    const relation = !existing ? 'fresh' : cmp(existing, v) < 0 ? 'update' : cmp(existing, v) === 0 ? 'same' : 'newer';
    return {
      version: v, relation, destination: '/Applications', destinationName: 'Applications', login: true, running: !!existing,
      existing: existing ? { version: existing, path: '/Applications/Switcher.app' } : null,
    };
  }
  function demoResult() {
    return { version: env?.version, path: '/Applications/Switcher.app', running: params.get('running') !== '0', cli: { path: '/usr/local/bin/switcher', dir: '/usr/local/bin', onPath: true }, login: true, skill: env?.relation === 'fresh', warnings: params.get('running') === '0' ? ['Switcher has not answered yet; open it from Applications if it does not appear in the menu bar.'] : [] };
  }
  function demoError() {
    return { title: 'Switcher was not installed', message: 'The copy of Switcher in this installer did not pass its check, so it was not installed.', detail: 'codesign --verify: a sealed resource is missing or invalid\n/Applications/.switcher-install-x1/Switcher.app', changed: false };
  }
  let mockTimer = null;
  function demoInstall() {
    const ids = ['copy', 'check', 'place', ...(env?.running ? ['stop'] : []), 'launch', 'cli', ...(env?.relation === 'fresh' ? ['skill'] : []), 'login'];
    receive({ type: 'plan', steps: ids });
    let i = 0;
    const fail = params.get('fail');
    const next = () => {
      if (i > 0) receive({ type: 'step', id: ids[i - 1], state: 'done' });
      if (i === ids.length) return params.get('screen') === 'install' ? null : receive({ type: 'done', result: demoResult() });
      const id = ids[i++];
      receive({ type: 'step', id, state: 'active' });
      if (id === fail) { receive({ type: 'step', id, state: 'failed' }); return receive({ type: 'error', ...demoError() }); }
      mockTimer = setTimeout(next, 300 + Math.random() * 900);
    };
    clearTimeout(mockTimer);
    next();
  }
  function mock(msg) {
    if (msg.type === 'ready') return setTimeout(() => receive({ type: 'env', env: mockEnv() }), 30);
    if (msg.type === 'install') return demoInstall();
    if (msg.type === 'copy') return navigator.clipboard?.writeText(msg.text).catch(() => {});
    console.log('installer:', msg);
  }

  startSparks();
  send({ type: 'ready' });
})();
