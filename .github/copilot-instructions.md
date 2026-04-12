# SOBS Copilot Instructions

## Stack
Flask/Jinja2, Bootstrap 5, chDB. Templates extend `base.html` via `{% block styles %}`, `{% block content %}`, `{% block scripts %}`.

---

## Design Constants

Use these exact values throughout — do not hardcode in individual templates:
- **Mobile breakpoint token:** `{{ mobile_breakpoint_max }}` (currently resolves to `575.98px` from `app.py`)
- **Mobile breakpoint CSS usage:** `@media (max-width: {{ mobile_breakpoint_max }})`
- **Mobile breakpoint JS usage:** `window.matchMedia('(max-width: {{ mobile_breakpoint_max }})')`
- **Test viewports:** 375px (small mobile), 480px (mobile), 575px (trigger point), 992px (tablet), 1440px (desktop)
- **Table border color:** `var(--bs-border-color)` (Bootstrap CSS variable)
- **Secondary text color:** `var(--bs-secondary-color)` (Bootstrap CSS variable)
- **Icon margin utility:** `me-1` (margin-right, Bootstrap) — override with `margin-right: 0 !important` at mobile

---

## Responsive UI Rules (apply to every template change)

### CSS Block Inheritance (MANDATORY)
**Every page template must have this structure — non-negotiable:**
```jinja2
{% block styles %}{{ super() }}<style>
  /* Page-specific CSS here */
</style>{% endblock %}
```
- If you forget `{{ super() }}`, shared stylesheet rules won't apply.
- `base.html` has `{% block styles %}{% endblock %}` before `</head>` — do not remove it.

### Tables
- **Every data table** must support mobile card mode at `≤{{ mobile_breakpoint_max }}`.
- Server-rendered tables: Add a shared CSS class (e.g., `tags-mobile-card-table`) and apply the standard mobile-card pattern:
  ```css
  @media (max-width: {{ mobile_breakpoint_max }}) {
    .tags-mobile-card-table thead { display: none; }
    .tags-mobile-card-table,
    .tags-mobile-card-table tbody,
    .tags-mobile-card-table tr,
    .tags-mobile-card-table td { display: block; width: 100%; }
    .tags-mobile-card-table tr { border: 1px solid var(--bs-border-color); border-radius: 0.5rem; margin-bottom: 0.75rem; padding: 0.5rem 0.625rem; }
    .tags-mobile-card-table td { border: 0; padding: 0.2rem 0; }
    .tags-mobile-card-table td::before { content: attr(data-label); display: block; font-size: 0.72rem; color: var(--bs-secondary-color); text-transform: uppercase; letter-spacing: 0.02em; margin-bottom: 0.1rem; }
  }
  ```
- **Every `<td>` must carry a `data-label="Column Name"` attribute** — this is how the label appears on mobile above the cell value.
- **JS-rendered tables** must also include the mobile-card class on the table and `data-label` attributes on each generated `<td>`. Use string concatenation or template literals to ensure these are present.

### Action Buttons (header/panel buttons)
- **Every action button** (Add, Delete, Edit, etc.) must wrap its label text in `<span class="PAGE-btn-label">Label</span>` and include a `title="Label"` attribute for accessibility.
- Hide labels at mobile (`≤{{ mobile_breakpoint_max }}`), showing only the icon:
  ```css
  @media (max-width: {{ mobile_breakpoint_max }}) {
    .PAGE-btn-label { display: none; }
    .btn:has(> .PAGE-btn-label) i { margin-right: 0 !important; }
  }
  ```
- **JS code that resets button `innerHTML`** (e.g., after async operations) must also use the `<span class="PAGE-btn-label">` wrapper.
- Example button HTML:
  ```html
  <button class="btn btn-sm btn-outline-info" title="Add Tag">
    <i class="bi bi-plus-circle me-1"></i><span class="PAGE-btn-label">Add Tag</span>
  </button>
  ```

### Page Headers
- **Every full page header** must use the shared macro in `templates/_page_header_macros.html` (`render_page_header`) unless there is a documented one-off exception.
- Preferred structure:
  ```jinja2
  {% from "_page_header_macros.html" import render_page_header %}
  {% set page_actions %}
    <a class="btn btn-sm btn-outline-secondary page-help-btn" title="Help">
      <i class="bi bi-question-circle me-1"></i><span class="PAGE-btn-label">Help</span>
    </a>
  {% endset %}
  {{ render_page_header('Page Title', icon_class='bi bi-ICON', icon_text_class='text-COLOR', actions_html=page_actions) }}
  ```
- **Mobile layout (`≤{{ mobile_breakpoint_max }}`)**:
  - Row 1: page icon + title on the left, action icon buttons on the right.
  - Row 2: meta line only (counts, chips, badges, refresh controls).
  - Optional subtext renders below the meta line.
- **Larger layout**:
  - Row 1: page icon + title, then meta line, then action buttons.
  - Row 2: subtext only when the page has explanatory copy.
- **Help button rule**: Help belongs at the far right of the action cluster, always uses the same bordered style (`page-help-btn` / outline-secondary), and uses the same question-circle icon across pages.
- **Back button rule**: Add a Back button only on secondary/detail/help pages with a clear parent page. Do not add Back on primary landing pages.
- **Meta-line priority**: row counts/status chips/refresh controls belong in the meta line, not mixed into the title text.

### No Horizontal Overflow
- Never use fixed pixel widths that exceed the viewport (test at 375px).
- Prefer `w-100`, `max-width: 100%`, `table-responsive` wrappers.
- Use `word-break: break-word` or `word-wrap: break-word` on long text cells (URLs, IDs, etc.).
- If containers or cards use flex with fixed widths, ensure the flex-wrap is set to wrap or items have `min-width: 0` to prevent overflow.

### Modals
- Always include `modal-dialog-scrollable` on dialogs with dynamic/long content (forms, lists, previews).
- On mobile, prefer `modal-fullscreen-sm-down` for complex forms to give maximum screen space.
- Ensure `z-index` doesn't cause modals to be clipped behind the sidebar or overlays.

### Theme Support (Light/Dark)
- Every UI change must work in both light and dark mode.
- Do not hardcode colors; use Bootstrap/theme variables (`var(--bs-*)`) and existing utility classes.
- Validate contrast and readability for text, icons, borders, badges, tables, and form states in both themes.
- Verify hover, focus, disabled, and active states remain visible in both themes.
- For custom charts/JS-rendered UI, read current theme and apply matching palette tokens instead of fixed hex values.

### General
- Check every interactive element (dropdowns, collapse panels, tooltips, autocomplete) renders correctly at 375px.
- Prefer Bootstrap utility classes over custom CSS where possible.
- **Before committing:** audit all tables, buttons, modals, and text-heavy elements for mobile responsiveness.
- Use Playwright visual tests with multiple viewports (375px, 480px, 575px, 1440px) to confirm mobile card mode and layout behavior.

---

## Code Reuse

- **Jinja2 macros/includes**: Repeated HTML structures (form fields, table rows, card layouts, modals) must live in `templates/` partials and be pulled in with `{% include %}` or `{% from ... import %}`. Do not duplicate markup across pages.
- **Shared JS**: Utility functions used across more than one page belong in a dedicated static JS file (e.g. `static/sobs-utils.js`), included once in `base.html`. Do not copy-paste JS helpers between `{% block scripts %}` blocks.
- **Shared CSS**: Page-agnostic CSS classes (e.g. mobile-card table rules) belong in a shared stylesheet, not repeated per-page. Per-page `{% block styles %}` is for page-specific overrides only.
- **Before adding new code**: search the existing templates, macros, and static files for an existing implementation. Reuse or extend it rather than creating a duplicate.
- **Table row rendering**: prefer server-rendered Jinja2 partials (`{% include %}`) or existing JS render helpers over writing new inline HTML strings per-page. Check `templates/` for any existing row/card partial before creating a new one.

## API / Backend Guidelines

- **Code reuse first**: prefer existing services/helpers/query builders/utilities before adding new logic; avoid duplicate API/business rules.
- **Minimal changes**: keep diffs focused on the requested behavior; avoid opportunistic refactors unless required for correctness or security.
- **Security first**: validate and sanitize all inputs, use parameterized queries, avoid exposing secrets/PII in responses or logs, and enforce authz checks on every protected route.
- **Limits and bounds**: enforce sane defaults and maximums for pagination, validate numeric/time ranges, cap payload sizes, and guard expensive queries/loops.
- **Prefer streaming for large outputs**: when response size can be large (exports, logs, long result sets), prefer streaming/chunked delivery patterns where feasible.
- **Stable API contracts (best practice)**: use consistent response/error shapes and correct HTTP status codes so clients can reliably parse and recover.
- **Python quality gate**: for Python code changes, run formatting/lint/type checks before finishing:
  `py_files=(${(f)"$(git ls-files '*.py')"}) && isort $py_files && black $py_files && flake8 $py_files && mypy $py_files`

## Timezone Support

Any page displaying timestamps **must** use the shared TZ system:
- Store all date/time values in UTC at rest (DB/files/events/logs) for consistency; timezone conversion is display-only.
- Add `class="sobs-tz-ts"` and `data-utc-ts="{{ value }}"` to every timestamp element (renders UTC by default, converted client-side).
- Wire up via the `render_tz_init_script` macro from `templates/_filter_macros.html`:
  ```jinja2
  {% from '_filter_macros.html' import render_tz_init_script %}
  <script>{{ render_tz_init_script('initMyPageTz', 'filtersPanelId', 'tzBtnId', 'tzLabelId') }}</script>
  ```
- If the page has no filters panel, call `window.sobsTimezone.initPage({ timestampSelector: '.sobs-tz-ts[data-utc-ts]' })` directly on `load`.
- JS-rendered timestamps must also emit `<span class="sobs-tz-ts" data-utc-ts="...">...</span>` and call the TZ re-render after injecting HTML.
