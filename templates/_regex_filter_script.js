// Shared regex filter IntelliSense/autocomplete for Logs, Errors, Traces, Metrics, and RUM pages.
// Call _sobsInitRegexFilter({ inputId, dropdownId, validateUrl, noMatchMessage, hintMessage, scope })
// after including this script to activate the filter on a given page.
function _sobsInitRegexFilter(opts) {
  const input = document.getElementById(opts.inputId);
  const dropdown = document.getElementById(opts.dropdownId);
  if (!input || !dropdown) return;

  const validateUrl = opts.validateUrl;
  const noMatchMessage = opts.noMatchMessage || "Valid regex — no matching records found.";
  const hintMessage = opts.hintMessage || "Enter an RE2 regex pattern to filter records.";
  const scope = (opts.scope && typeof opts.scope === "object") ? opts.scope : null;
  const suggestionLimit = Number.isFinite(opts.suggestionLimit) ? Math.max(5, Number(opts.suggestionLimit)) : 30;

  let activeIdx = -1;
  let lastItems = [];
  let validateTimer = null;
  let validateSeq = 0;
  let validateController = null;
  let statusState = null;

  // Common regex constructs to suggest.
  const REGEX_SNIPPETS = [
    { label: "\\d",     hint: "digit [0-9]",                insert: "\\d",     ctx: ["\\"] },
    { label: "\\d+",    hint: "one or more digits",         insert: "\\d+",    ctx: ["\\", "general"] },
    { label: "\\d{n,m}",hint: "digit count range",          insert: "\\d{",    ctx: ["\\"] },
    { label: "\\w",     hint: "word char [a-zA-Z0-9_]",    insert: "\\w",     ctx: ["\\"] },
    { label: "\\w+",    hint: "one or more word chars",     insert: "\\w+",    ctx: ["\\", "general"] },
    { label: "\\s",     hint: "whitespace",                 insert: "\\s",     ctx: ["\\"] },
    { label: "\\s+",    hint: "one or more whitespace",     insert: "\\s+",    ctx: ["\\"] },
    { label: "\\S+",    hint: "one or more non-whitespace", insert: "\\S+",    ctx: ["\\"] },
    { label: "\\D+",    hint: "one or more non-digits",     insert: "\\D+",    ctx: ["\\"] },
    { label: "\\W+",    hint: "one or more non-word chars", insert: "\\W+",    ctx: ["\\"] },
    { label: "\\n",     hint: "newline",                    insert: "\\n",     ctx: ["\\"] },
    { label: "\\t",     hint: "tab",                        insert: "\\t",     ctx: ["\\"] },
    { label: "\\b",     hint: "word boundary",              insert: "\\b",     ctx: ["\\"] },
    { label: "\\A",     hint: "start of text",              insert: "\\A",     ctx: ["\\"] },
    { label: "\\z",     hint: "end of text",                insert: "\\z",     ctx: ["\\"] },
    { label: "\\.",     hint: "literal dot",                insert: "\\.",     ctx: ["\\", "general"] },
    { label: "\\/",     hint: "literal slash",              insert: "\\/",     ctx: ["\\", "general"] },
    { label: "[[:digit:]]+", hint: "POSIX digits",            insert: "[[:digit:]]+", ctx: ["[", "general"] },
    { label: "[[:alpha:]]+", hint: "POSIX letters",           insert: "[[:alpha:]]+", ctx: ["[", "general"] },
    { label: "[[:alnum:]_]+", hint: "POSIX word-like",        insert: "[[:alnum:]_]+", ctx: ["[", "general"] },
    { label: "[[:space:]]+", hint: "POSIX whitespace",        insert: "[[:space:]]+", ctx: ["[", "general"] },
    { label: "[a-z]",   hint: "lowercase letters",          insert: "[a-z]",   ctx: ["[", "general"] },
    { label: "[A-Z]",   hint: "uppercase letters",          insert: "[A-Z]",   ctx: ["["] },
    { label: "[0-9]",   hint: "digits (explicit)",          insert: "[0-9]",   ctx: ["["] },
    { label: "[a-zA-Z]",hint: "any letter",                 insert: "[a-zA-Z]",ctx: ["["] },
    { label: "[^...]",  hint: "any char except ...",        insert: "[^",      ctx: ["["] },
    { label: "(?:...)", hint: "non-capturing group",        insert: "(?:",     ctx: ["(", "general"] },
    { label: "(?i)...", hint: "case-insensitive prefix",    insert: "(?i)",    ctx: ["("] },
    { label: "(a|b)",   hint: "alternation",                insert: "(",       ctx: ["("] },
    { label: "{n}",     hint: "exactly n times",            insert: "{",       ctx: ["{"] },
    { label: "{n,}",    hint: "at least n times",           insert: "{",       ctx: ["{"] },
    { label: "{n,m}",   hint: "between n and m times",      insert: "{",       ctx: ["{"] },
    { label: "*?",      hint: "lazy zero or more",           insert: "*?",      ctx: ["general"] },
    { label: "+?",      hint: "lazy one or more",            insert: "+?",      ctx: ["general"] },
    { label: "??",      hint: "lazy optional",               insert: "??",      ctx: ["general"] },
    { label: "^",       hint: "start of string",            insert: "^",       ctx: ["^"] },
    { label: "$",       hint: "end of string",              insert: "$",       ctx: ["$"] },
    { label: ".*",      hint: "any chars (greedy)",         insert: ".*",      ctx: ["general"] },
    { label: ".*?",     hint: "any chars (lazy)",           insert: ".*?",     ctx: ["general"] },
    { label: ".+",      hint: "one or more any chars",      insert: ".+",      ctx: ["general"] },
    { label: ".+?",     hint: "one or more any chars (lazy)", insert: ".+?",   ctx: ["general"] },
    { label: "(?:error|warn|fatal)", hint: "grouped alternation", insert: "(?:error|warn|fatal)", ctx: ["general"] },
    { label: "(?m)^ERROR", hint: "multiline line-start match", insert: "(?m)^ERROR", ctx: ["general", "("] },
    { label: "error|warn", hint: "error/warn terms",        insert: "error|warn", ctx: ["general"] },
    { label: "exception|error|fatal", hint: "error type terms", insert: "exception|error|fatal", ctx: ["general"] },
    { label: "\\d{4}-\\d{2}-\\d{2}", hint: "date YYYY-MM-DD", insert: "\\d{4}-\\d{2}-\\d{2}", ctx: ["general"] },
    { label: "\\d+\\.\\d+", hint: "decimal number", insert: "\\d+\\.\\d+", ctx: ["general"] },
    { label: "(?:GET|POST|PUT|DELETE)", hint: "HTTP methods", insert: "(?:GET|POST|PUT|DELETE)", ctx: ["general"] },
    { label: "\\b(?:5\\d\\d|4\\d\\d)\\b", hint: "HTTP 4xx/5xx status", insert: "\\b(?:5\\d\\d|4\\d\\d)\\b", ctx: ["general"] },
    { label: "(?:timeout|timed\\s*out|deadline)", hint: "timeout variants", insert: "(?:timeout|timed\\s*out|deadline)", ctx: ["general"] },
    { label: "https?://\\S+", hint: "URL",                  insert: "https?://\\S+", ctx: ["general"] },
    { label: "\\b\\d{1,3}(\\.\\d{1,3}){3}\\b", hint: "IPv4 address", insert: "\\b\\d{1,3}(\\.\\d{1,3}){3}\\b", ctx: ["general"] },
  ];

  function _esc(s) {
    return String(s).replace(/&/g,"&amp;").replace(/</g,"&lt;").replace(/>/g,"&gt;");
  }

  function renderStatusRow() {
    const existing = dropdown.querySelector("li[data-status]");
    if (existing) existing.remove();
    if (!statusState) return;
    const li = document.createElement("li");
    li.setAttribute("data-status", "true");
    li.setAttribute("role", "presentation");
    const colorClass = statusState.level === "error" ? "text-danger"
      : statusState.level === "success" ? "text-success"
      : "text-secondary";
    li.className = `px-2 py-1 small border-bottom ${colorClass}`;
    li.style.cssText = "cursor:default;user-select:none;";
    li.textContent = statusState.message;
    dropdown.insertBefore(li, dropdown.firstChild);
  }

  function setStatusRow(level, message) {
    statusState = message ? { level, message } : null;
    if (dropdown.style.display !== "none") renderStatusRow();
  }

  function clientValidate(value) {
    const v = String(value || "").trim();
    if (!v) return { ok: true, level: "info", message: hintMessage };
    try {
      new RegExp(v, "i");
      return { ok: true, level: "success", message: "Valid regex pattern." };
    } catch (err) {
      return { ok: false, level: "error", message: `Invalid regex: ${err.message}` };
    }
  }

  async function validateServerSide() {
    const requestSeq = ++validateSeq;
    const patternSnapshot = input.value;
    const local = clientValidate(patternSnapshot);
    setStatusRow(local.level, local.message);
    if (!local.ok || !patternSnapshot.trim()) return;

    if (validateController) validateController.abort();
    validateController = new AbortController();

    try {
      const res = await fetch(validateUrl, {
        method: "POST",
        credentials: "same-origin",
        signal: validateController.signal,
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(scope ? { pattern: patternSnapshot, scope } : { pattern: patternSnapshot }),
      });
      if (!res.ok) return;
      const data = await res.json();
      if (requestSeq !== validateSeq || input.value !== patternSnapshot) return;
      if (!data.ok) {
        setStatusRow("error", `Invalid regex: ${data.error || "pattern error"}`);
      } else if (data.sample) {
        const preview = data.sample.length > 120 ? data.sample.slice(0, 117) + "…" : data.sample;
        setStatusRow("success", `✓ Matches e.g.: ${preview}`);
      } else {
        setStatusRow("success", noMatchMessage);
      }
    } catch (err) {
      if (err && err.name === "AbortError") return;
    } finally {
      if (requestSeq === validateSeq) validateController = null;
    }
  }

  function getContext(val, cursor) {
    const text = val.slice(0, cursor);
    if (!text) return "general";
    const last = text[text.length - 1];
    if (last === "\\") return "\\";
    if (last === "[")  return "[";
    if (last === "(")  return "(";
    if (last === "{")  return "{";
    if (last === "^" && text.length === 1) return "^";
    if (last === "$")  return "$";
    return "general";
  }

  function closeDropdown() {
    dropdown.style.display = "none";
    dropdown.innerHTML = "";
    activeIdx = -1;
    lastItems = [];
  }

  function openDropdown(items) {
    lastItems = items;
    activeIdx = -1;
    dropdown.innerHTML = "";
    items.forEach((item) => {
      const li = document.createElement("li");
      li.setAttribute("role", "option");
      li.setAttribute("aria-selected", "false");
      li.className = "dropdown-item py-1 px-2";
      li.style.cursor = "pointer";
      li.innerHTML = `<strong>${_esc(item.label)}</strong>` +
        (item.hint ? ` <small class="text-muted ms-1">${_esc(item.hint)}</small>` : "");
      li.addEventListener("mousedown", (e) => { e.preventDefault(); applyItem(item); });
      dropdown.appendChild(li);
    });
    if (!items.length && !statusState) { dropdown.style.display = "none"; return; }
    dropdown.style.display = "block";
    renderStatusRow();
  }

  function setActive(idx) {
    const lis = dropdown.querySelectorAll("li[role='option']");
    lis.forEach((li, i) => {
      li.classList.toggle("active", i === idx);
      li.setAttribute("aria-selected", i === idx ? "true" : "false");
    });
    activeIdx = idx;
    if (idx >= 0 && lis[idx]) lis[idx].scrollIntoView({ block: "nearest" });
  }

  function applyItem(item) {
    const val = input.value;
    const cursor = input.selectionStart;
    const before = val.slice(0, cursor);
    const after = val.slice(cursor);
    const singleCharTriggers = ["\\", "[", "(", "{", "^", "$"];
    const lastChar = before[before.length - 1];
    let newBefore;
    if (singleCharTriggers.includes(lastChar) && item.insert.startsWith(lastChar)) {
      newBefore = before + item.insert.slice(1);
    } else {
      newBefore = before.replace(/[\w.*+?^${}()|\[\]\\]*$/, item.insert);
    }
    input.value = newBefore + after;
    const newCursor = newBefore.length;
    input.setSelectionRange(newCursor, newCursor);
    closeDropdown();
    input.focus();
    const local = clientValidate(input.value);
    setStatusRow(local.level, local.message);
  }

  function suggest() {
    const cursor = input.selectionStart;
    const ctx = getContext(input.value, cursor);
    const text = input.value.slice(0, cursor);
    const tokenMatch = text.match(/([\w.*+?^${}()|\[\]\\]*)$/);
    const pfx = (tokenMatch ? tokenMatch[1] : "").toLowerCase();

    let items;
    if (ctx === "general") {
      items = REGEX_SNIPPETS
        .filter((s) => s.ctx.includes("general"))
        .filter((s) => !pfx || s.label.toLowerCase().startsWith(pfx) || s.insert.toLowerCase().startsWith(pfx));
    } else {
      items = REGEX_SNIPPETS.filter((s) => s.ctx.includes(ctx));
    }
    openDropdown(items.slice(0, suggestionLimit));
  }

  input.addEventListener("input", () => {
    const local = clientValidate(input.value);
    setStatusRow(local.level, local.message);
    suggest();
    if (validateTimer) window.clearTimeout(validateTimer);
    validateTimer = window.setTimeout(validateServerSide, 400);
  });

  input.addEventListener("focus", () => {
    const local = clientValidate(input.value);
    setStatusRow(local.level, local.message);
    suggest();
    if (input.value.trim()) {
      if (validateTimer) window.clearTimeout(validateTimer);
      validateTimer = window.setTimeout(validateServerSide, 100);
    }
  });

  input.addEventListener("blur", () => {
    if (validateTimer) window.clearTimeout(validateTimer);
    validateTimer = window.setTimeout(validateServerSide, 100);
    window.setTimeout(closeDropdown, 150);
  });

  input.addEventListener("keydown", (e) => {
    if (dropdown.style.display === "none") return;
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive(Math.min(activeIdx + 1, lastItems.length - 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive(Math.max(activeIdx - 1, -1));
    } else if (e.key === "Enter" || e.key === "Tab") {
      if (activeIdx >= 0 && lastItems[activeIdx]) {
        e.preventDefault();
        applyItem(lastItems[activeIdx]);
      } else {
        closeDropdown();
      }
    } else if (e.key === "Escape") {
      closeDropdown();
    }
  });

  document.addEventListener("click", (e) => {
    if (!input.contains(e.target) && !dropdown.contains(e.target)) closeDropdown();
  });

  const initial = clientValidate(input.value);
  if (input.value.trim()) {
    statusState = { level: initial.level, message: initial.message };
    if (initial.ok) {
      validateTimer = window.setTimeout(validateServerSide, 200);
    }
  }
}
