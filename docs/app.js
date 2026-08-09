/**
 * Conduit landing page — terminal typewriter, copy-to-clipboard, scroll reveal.
 * No build step, no dependencies.
 */

const REDUCED_MOTION = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

/** @type {Array<{text: string, cls?: string}>} Lines rendered into the hero terminal. */
const TERMINAL_LINES = [
  { text: '$ docker run -p 8080:8080 -p 8081:8081 conduit', cls: 'prompt' },
  { text: '[Gateway] Starting API Gateway on :8080' },
  { text: '[Mock Server] Starting on :8081' },
  { text: '[Gateway] GET /v1/connectors - 76µs' },
  { text: '' },
  { text: '$ curl localhost:8080/v1/connectors', cls: 'prompt' },
  { text: '[{"id":"slack",...},{"id":"github",...},{"id":"stripe",...}]' },
];

/** Types the hero terminal lines one character at a time; skips straight to full text under reduced motion. */
async function typeTerminal() {
  const el = document.getElementById('terminal-code');
  if (!el) return;

  if (REDUCED_MOTION) {
    el.textContent = TERMINAL_LINES.map(l => l.text).join('\n');
    return;
  }

  for (const line of TERMINAL_LINES) {
    const lineSpan = document.createElement('div');
    if (line.cls) lineSpan.className = line.cls;
    el.appendChild(lineSpan);

    for (const char of line.text) {
      lineSpan.textContent += char;
      await sleep(line.cls === 'prompt' ? 22 : 4);
    }
    await sleep(120);
  }
}

/** @param {number} ms */
function sleep(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}

/**
 * Fades feature cards in as they enter the viewport. Cards are visible by
 * default (see style.css) — this only opts cards currently below the fold
 * into a hidden-then-reveal state, so a card can never end up permanently
 * invisible if IntersectionObserver doesn't fire for it.
 */
function initScrollReveal() {
  const cards = document.querySelectorAll('.feature-card');
  if (REDUCED_MOTION || !('IntersectionObserver' in window)) return;

  const reveal = card => {
    card.classList.add('in-view');
    card.classList.remove('pre-reveal');
    observer.unobserve(card);
  };

  const observer = new IntersectionObserver(entries => {
    entries.forEach(entry => { if (entry.isIntersecting) reveal(entry.target); });
  }, { threshold: 0.15 });

  cards.forEach((card, i) => {
    if (card.getBoundingClientRect().top > window.innerHeight) {
      card.classList.add('pre-reveal');
      card.style.animationDelay = `${(i % 6) * 60}ms`;
      observer.observe(card);
      // Safety net: guarantee visibility even if IntersectionObserver never
      // fires for this card (e.g. a full-page capture tool that resizes the
      // viewport without dispatching a scroll/layout event in time).
      setTimeout(() => reveal(card), 1200);
    }
  });
}

function initCopyButton() {
  const btn = document.getElementById('copy-btn');
  const code = document.getElementById('quickstart-code');
  if (!btn || !code) return;

  btn.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(code.textContent.trim());
      btn.textContent = 'Copied!';
    } catch {
      btn.textContent = 'Press ⌘C to copy';
    }
    setTimeout(() => { btn.textContent = 'Copy'; }, 1800);
  });
}

typeTerminal();
initScrollReveal();
initCopyButton();
