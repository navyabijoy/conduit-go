/**
 * Conduit landing page: hero terminal playback and copy-to-clipboard.
 * No build step, no dependencies.
 */

const REDUCED_MOTION = window.matchMedia('(prefers-reduced-motion: reduce)').matches;

/**
 * Gateway session shown in the hero terminal. Log lines mirror the real
 * log.Printf formats in gateway/server.go, gateway/drift.go and
 * mock/mock_server.go, so the page only shows what the binary prints.
 *
 * A line is either a typed command ({ cmd }), a blank spacer ({ gap: true }),
 * or a log line ({ t, tag, parts }) where parts are [text, className?] pairs.
 * @type {Array<{cmd?: string, gap?: boolean, t?: string, tag?: string, parts?: Array<[string, string?]>, pause?: number}>}
 */
const SESSION = [
  { cmd: 'docker run -p 8080:8080 -p 8081:8081 conduit' },
  { t: '14:02:07', tag: '[Gateway]', parts: [['Starting API Gateway on :8080']] },
  { t: '14:02:07', tag: '[Mock Server]', parts: [['Starting on :8081']], pause: 500 },
  { gap: true },
  { t: '14:02:11', tag: '[Gateway]', parts: [['Executing slack.send_message for instance inst_slack_9f3a1c2e7b04d815']] },
  { t: '14:02:11', tag: '[Gateway]', parts: [['Refreshing OAuth token', 'hl-accent'], [' for instance inst_slack_9f3a1c2e7b04d815']], pause: 420 },
  { t: '14:02:12', tag: '[Gateway]', parts: [['Token refreshed successfully', 'hl-ok'], [' for instance inst_slack_9f3a1c2e7b04d815']], pause: 700 },
  { gap: true },
  { t: '14:02:37', tag: '[Drift Detector]', parts: [['Scanning instance inst_github_3c07e5a1d29b6f40 (github) for schema drift...']], pause: 600 },
  { t: '14:02:37', tag: '[DRIFT DETECTED]', tagCls: 'hl-alert', parts: [['Instance inst_github_3c07e5a1d29b6f40 (github) has drifted! Details: Field ".[].state" removed']] },
];

/** @param {string} text @param {string} [cls] */
function span(text, cls) {
  const el = document.createElement('span');
  if (cls) el.className = cls;
  el.textContent = text;
  return el;
}

/** Builds one terminal line element; commands start empty so they can be typed. */
function buildLine(line) {
  const el = document.createElement('span');
  if (line.gap) {
    el.className = 'line gap';
    return { el };
  }
  if (line.cmd !== undefined) {
    el.className = 'line cmd';
    el.append(span('$ ', 'ps'));
    const text = span('');
    el.append(text);
    return { el, text };
  }
  el.className = 'line';
  el.append(span(line.t + ' ', 't'), span(line.tag, line.tagCls || 'tag'), ' ');
  for (const [text, cls] of line.parts) el.append(cls ? span(text, cls) : text);
  return { el };
}

/** Plays the session: commands type out, log lines arrive one at a time. */
async function playTerminal() {
  const root = document.getElementById('terminal-code');
  if (!root) return;

  for (const line of SESSION) {
    const { el, text } = buildLine(line);
    if (REDUCED_MOTION) {
      if (text) text.textContent = line.cmd;
      root.append(el);
      continue;
    }
    root.append(el);
    if (text) {
      for (const char of line.cmd) {
        text.textContent += char;
        await sleep(24);
      }
      await sleep(400);
    } else if (!line.gap) {
      await sleep(line.pause ?? 160);
    }
  }
}

/** @param {number} ms */
function sleep(ms) {
  return new Promise(resolve => setTimeout(resolve, ms));
}

function initCopyButton() {
  const btn = document.getElementById('copy-btn');
  const code = document.getElementById('quickstart-code');
  if (!btn || !code) return;

  btn.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(code.textContent.trim());
      btn.textContent = 'Copied';
    } catch {
      btn.textContent = 'Press ⌘C to copy';
    }
    setTimeout(() => { btn.textContent = 'Copy'; }, 1800);
  });
}

playTerminal();
initCopyButton();
