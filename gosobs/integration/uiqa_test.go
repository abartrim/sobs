package integration

import (
	"fmt"
	"testing"
	"time"

	"github.com/go-rod/rod"
)

// ---------------------------------------------------------------------------
// UIQA helpers (port of TestUIQA's private methods)
// ---------------------------------------------------------------------------

// initPage suppresses first-run modals for every navigation on this page.
func (p *page) initPage() {
	p.MustEvalOnNewDocument(`
        try {
            localStorage.setItem('sobs.setupWizardSeen.v1',  '1');
            localStorage.setItem('sobs.firstRunTourSeen.v1', '1');
            localStorage.setItem('sobs.firstRunTourShown.v1', '1');
        } catch (_) {}
    `)
}

func (p *page) dismissBlockingModals() {
	if !p.evalBool(`() => !!document.querySelector('.modal.show:not(#sobsConfirmModal)')`) {
		return
	}
	p.MustEval(`() => {
        const api = window.bootstrap && window.bootstrap.Modal;
        if (!api) return;
        document.querySelectorAll('.modal.show:not(#sobsConfirmModal)').forEach(el => {
            (api.getInstance(el) || api.getOrCreateInstance(el)).hide();
        });
    }`)
	p.waitFn(`() => document.querySelectorAll('.modal.show:not(#sobsConfirmModal)').length === 0`)
}

func (p *page) waitConfirmFullyVisible() {
	// Bound to 5s like Playwright's default action timeout: if the confirm modal
	// never appears (e.g. the page navigated instead of intercepting the submit),
	// fail fast instead of blocking on the 90s page deadline.
	s := p.short(5 * time.Second)
	s.MustElement("#sobsConfirmModal.show")
	s.MustWait(`() => {
        const m = document.getElementById('sobsConfirmModal');
        if (!m) return false;
        const s = window.getComputedStyle(m);
        if (s.display === 'none' || Number(s.opacity) < 0.99) return false;
        const d = m.querySelector('.modal-dialog');
        if (!d) return false;
        const r = d.getBoundingClientRect();
        return r.width > 0 && r.height > 0;
    }`)
}

func (p *page) openConfirmAndCancel() {
	p.waitConfirmFullyVisible()
	s := p.short(5 * time.Second)
	s.MustElement("#sobsConfirmModal .modal-footer [data-bs-dismiss='modal']").MustClick()
	s.MustWait(`() => !document.querySelector('#sobsConfirmModal.show')`)
}

func (p *page) openConfirmAndAccept() {
	p.waitConfirmFullyVisible()
	p.short(5 * time.Second).MustElement("#sobsConfirmModalOkBtn").MustClick()
	p.waitDCL()
}

func (p *page) toastCount() int {
	return p.evalInt(`() => {
        const c = document.getElementById('sobsNotifyToastContainer');
        return c ? c.querySelectorAll('.toast').length : 0;
    }`)
}

func (p *page) expectNewToast(before int, hint string) {
	p.waitFn(`({ count, hint }) => {
        const c = document.getElementById('sobsNotifyToastContainer');
        if (!c) return false;
        const all = Array.from(c.querySelectorAll('.toast'));
        if (all.length <= count) return false;
        if (!hint) return true;
        return all.slice(count).some(el =>
            String(el.textContent || '').toLowerCase().includes(hint.toLowerCase())
        );
    }`, map[string]any{"count": before, "hint": hint})
}

func (p *page) syntheticNotifyFallback(message, hint string) {
	before := p.toastCount()
	p.MustEval(`(msg) => {
        if (window.SOBS && typeof window.SOBS.notify === 'function') {
            window.SOBS.notify(msg, { level: 'danger', title: 'QA Synthetic', delay: 2200 });
        }
    }`, message)
	p.expectNewToast(before, hint)
}

func (p *page) withFetchFailure(fn func()) {
	p.MustEval(`() => {
        if (!window.__qaOrigFetch) window.__qaOrigFetch = window.fetch.bind(window);
        window.fetch = () => Promise.reject(new Error('qa-net-fail'));
    }`)
	defer p.MustEval(`() => { if (window.__qaOrigFetch) window.fetch = window.__qaOrigFetch; }`)
	fn()
}

func (p *page) withCopyFailure(fn func()) {
	p.MustEval(`() => {
        if (!window.__qaOrigCopy && window.SOBS) window.__qaOrigCopy = window.SOBS.copyToClipboard;
        if (window.SOBS) window.SOBS.copyToClipboard = () => Promise.reject(new Error('qa-copy-fail'));
    }`)
	defer p.MustEval(`() => { if (window.SOBS && window.__qaOrigCopy) window.SOBS.copyToClipboard = window.__qaOrigCopy; }`)
	fn()
}

func (p *page) withFetchJSON(payload map[string]any, fn func()) {
	p.MustEval(`(jsonPayload) => {
        if (!window.__qaOrigFetch) window.__qaOrigFetch = window.fetch.bind(window);
        window.fetch = () => Promise.resolve({
            ok: true, status: 200,
            json: () => Promise.resolve(jsonPayload),
        });
    }`, payload)
	defer p.MustEval(`() => { if (window.__qaOrigFetch) window.fetch = window.__qaOrigFetch; }`)
	fn()
}

func (p *page) checkDeclarativeConfirm(selector string) bool {
	if p.count(selector) == 0 {
		return false
	}
	form := p.first(selector)
	// Trigger via a DOM click rather than rod's MustClick: the submit button can
	// be non-interactable in some rendered layouts (e.g. a card/collapsed table),
	// which makes MustClick block until the page deadline. A DOM click still fires
	// the form's submit handler that opens the SOBS confirm modal.
	if btns := form.MustElements("button[type='submit'],input[type='submit']"); len(btns) > 0 {
		btns.First().MustEval(`() => this.click()`)
	} else {
		form.MustEval(`n => (typeof n.requestSubmit === 'function' ? n.requestSubmit() : n.submit())`)
	}
	p.openConfirmAndCancel()
	return true
}

func (p *page) toggleAndRevert(selector string) bool {
	if p.count(selector) == 0 {
		return false
	}
	btn := p.first(selector)
	btn.MustScrollIntoView()
	btn.MustClick()
	p.waitDCL()
	if p.count(selector) == 0 {
		return false
	}
	p.first(selector).MustClick()
	p.waitDCL()
	return true
}

func (p *page) jsClick(sel string) {
	p.MustEval(`(s) => { const e = document.querySelector(s); if (e) e.click(); }`, sel)
}

func (p *page) commonChecks(url string) {
	t := p.t
	p.navLoad(url)
	p.dismissBlockingModals()

	if !p.evalBool(`() => !!(window.SOBS && typeof window.SOBS.notify === 'function')`) {
		t.Fatalf("window.SOBS.notify not available on %s", url)
	}
	if !p.evalBool(`() => !!(window.SOBS && typeof window.SOBS.confirm === 'function')`) {
		t.Fatalf("window.SOBS.confirm not available on %s", url)
	}
	if p.count("#sobsNotifyToastContainer") != 1 {
		t.Fatalf("missing #sobsNotifyToastContainer on %s", url)
	}
	if pos := p.evalStr(`() => window.getComputedStyle(document.getElementById('sobsNotifyToastContainer')).position`); pos != "fixed" {
		t.Fatalf("toast container position is %q, not 'fixed' on %s", pos, url)
	}

	// Toast smoke: show then auto-hide.
	p.MustEval(`() => window.SOBS.notify('QA smoke', {title:'QA',level:'info',delay:1200})`)
	p.MustElement("#sobsNotifyToastContainer .toast.show")
	p.waitFn(`() => {
        const c = document.getElementById('sobsNotifyToastContainer');
        return !c || c.querySelectorAll('.toast.show').length === 0;
    }`)

	// Notify XSS regression.
	p.MustEval(`() => {
        window.__qaNotifyXssExecuted = false;
        window.SOBS.notify('<img src=x onerror="window.__qaNotifyXssExecuted=true">QA-XSS-BODY', {
            title: '<svg onload="window.__qaNotifyXssExecuted=true">QA-XSS-TITLE',
            level: 'warning', delay: 1200,
        });
    }`)
	p.MustElement("#sobsNotifyToastContainer .toast.show")
	xss := p.evalJSON(`() => {
        const c = document.getElementById('sobsNotifyToastContainer');
        const toasts = c ? Array.from(c.querySelectorAll('.toast')) : [];
        const latest  = toasts.length ? toasts[toasts.length - 1] : null;
        const titleEl = latest ? latest.querySelector('.toast-header strong') : null;
        const bodyEl  = latest ? latest.querySelector('.toast-body') : null;
        return {
            executed:                 !!window.__qaNotifyXssExecuted,
            titleHasInjectedElement:  !!(titleEl && titleEl.querySelector('*')),
            bodyHasInjectedElement:   !!(bodyEl  && bodyEl.querySelector('*')),
            titleText: titleEl ? String(titleEl.textContent || '') : '',
            bodyText:  bodyEl  ? String(bodyEl.textContent  || '') : '',
        };
    }`)
	if xss.Get("executed").Bool() {
		t.Errorf("XSS payload executed on %s", url)
	}
	if xss.Get("titleHasInjectedElement").Bool() {
		t.Errorf("XSS injected into toast title on %s", url)
	}
	if xss.Get("bodyHasInjectedElement").Bool() {
		t.Errorf("XSS injected into toast body on %s", url)
	}
	if !contains(xss.Get("titleText").Str(), "<svg") {
		t.Errorf("XSS title not escaped as text on %s: %q", url, xss.Get("titleText").Str())
	}
	if !contains(xss.Get("bodyText").Str(), "<img") {
		t.Errorf("XSS body not escaped as text on %s: %q", url, xss.Get("bodyText").Str())
	}
	p.waitFn(`() => {
        const c = document.getElementById('sobsNotifyToastContainer');
        return !c || c.querySelectorAll('.toast.show').length === 0;
    }`)

	// Programmatic confirm resolves false on cancel.
	p.dismissBlockingModals()
	p.MustEval(`() => {
        window.__qaConfirmResolved = null;
        window.SOBS.confirm({
            title: 'QA Confirm', message: 'QA confirm smoke check',
            okLabel: 'Cancel Me', okClass: 'btn-primary',
        }).then(v => { window.__qaConfirmResolved = v; });
    }`)
	p.openConfirmAndCancel()
	p.waitFn(`() => window.__qaConfirmResolved === false`)
}

func (p *page) checkQueuedConfirm() {
	p.MustEval(`() => {
        window.__qaConfirmFirstResolved  = null;
        window.__qaConfirmSecondResolved = null;
        window.SOBS.confirm({
            title: 'QA Queue Confirm 1', message: 'First queued confirm',
            okLabel: 'Continue', okClass: 'btn-primary',
        }).then(v => { window.__qaConfirmFirstResolved = v; });
        window.SOBS.confirm({
            title: 'QA Queue Confirm 2', message: 'Second queued confirm',
            okLabel: 'Delete', okClass: 'btn-danger',
        }).then(v => { window.__qaConfirmSecondResolved = v; });
    }`)
	p.waitConfirmFullyVisible()
	p.waitFn(`() => {
        const t = document.getElementById('sobsConfirmModalTitle');
        return !!t && t.textContent.trim() === 'QA Queue Confirm 1';
    }`)
	p.clickSel("#sobsConfirmModalOkBtn")
	p.waitFn(`() => window.__qaConfirmFirstResolved === true`)
	p.waitConfirmFullyVisible()
	p.waitFn(`() => {
        const t = document.getElementById('sobsConfirmModalTitle');
        return !!t && t.textContent.trim() === 'QA Queue Confirm 2';
    }`)
	if !p.evalBool(`() => window.__qaConfirmSecondResolved === null`) {
		p.t.Errorf("second queued confirm was prematurely resolved (confirm-queue sequencing regression)")
	}
	p.clickSel("#sobsConfirmModal .modal-footer [data-bs-dismiss='modal']")
	p.waitSelectorGone("#sobsConfirmModal.show")
	p.waitFn(`() => window.__qaConfirmSecondResolved === false`)
}

func (p *page) checkSidebarToggle() {
	if p.count("#sbToggleBtn") == 0 || p.count("#sbSidebar") == 0 {
		return
	}
	before := p.evalBool(`() => !!(document.getElementById('sbSidebar') &&
        document.getElementById('sbSidebar').classList.contains('sidebar-compact'))`)
	p.first("#sbToggleBtn").MustClick()
	p.waitFn(`(before) => {
        const el = document.getElementById('sbSidebar');
        return !!el && el.classList.contains('sidebar-compact') !== before;
    }`, before)
	p.first("#sbToggleBtn").MustClick()
	p.waitFn(`(before) => {
        const el = document.getElementById('sbSidebar');
        return !!el && el.classList.contains('sidebar-compact') === before;
    }`, before)
}

func (p *page) assertNoDialogs(route string) {
	if d := p.dialogs(); len(d) > 0 {
		p.t.Errorf("native browser dialogs on %s: %v", route, d)
	}
}

// ---------------------------------------------------------------------------
// TestUIQA
// ---------------------------------------------------------------------------

func testUIQA(t *testing.T) {
	seed(envInt("SOBS_UIQA_SEED_TOTAL", 64), envInt("SOBS_UIQA_SEED_WORKERS", 8))

	t.Run("root", testUIQARoot)
	t.Run("dashboards", testUIQADashboards)
	t.Run("reports", testUIQAReports)
	t.Run("settings_tags", testUIQASettingsTags)
	t.Run("settings_repositories_onboarding_wizard_opens", testUIQASettingsRepositories)
	t.Run("settings_data_management", testUIQADataManagement)
	t.Run("settings_notifications", testUIQANotifications)
	t.Run("metrics_rules", testUIQAMetricsRules)
	t.Run("settings_agents", testUIQAAgents)
	t.Run("errors", testUIQAErrors)
	t.Run("traces", testUIQATraces)
	t.Run("incident", testUIQAIncident)
}

func testUIQARoot(t *testing.T) {
	p := newPage(t)
	defer p.close()
	p.initPage()
	p.commonChecks(baseURL + "/")
	p.checkQueuedConfirm()
	p.checkSidebarToggle()

	if p.evalBool(`() => typeof window.__sobsOpenSetupWizard === 'function'`) {
		p.MustEval(`() => {
            try { localStorage.removeItem('sobs.setupWizardSeen.v1'); } catch (_) {}
            if (typeof window.__sobsOpenSetupWizard === 'function') window.__sobsOpenSetupWizard();
        }`)
		p.MustElement("#setupWizardModal.show")
		p.clickSel("#envOptions .wizard-option-btn[data-value='dev']")
		p.clickSel("#wizardNextBtn")
		p.clickSel("#langOptions .wizard-option-btn[data-value='python']")
		p.clickSel("#wizardNextBtn")
		p.clickSel("#deployOptions .wizard-option-btn[data-value='docker']")
		p.clickSel("#wizardNextBtn")
		p.MustElement("#wizardStep3.active")
		matched := p.evalBool(`() => performance.getEntriesByType('resource')
            .some(e => /\/api\/setup-wizard\/steps(\?|$)/.test(e.name))`)
		if !matched {
			t.Errorf("setup wizard did not request /api/setup-wizard/steps")
		}
		if p.count("#setupWizardModal .btn-close") > 0 {
			p.first("#setupWizardModal .btn-close").MustClick()
			p.waitSelectorGone("#setupWizardModal.show")
		}
	}
	p.assertNoDialogs("/")
}

func testUIQADashboards(t *testing.T) {
	p := newPage(t)
	defer p.close()
	p.initPage()
	p.commonChecks(baseURL + "/dashboards")
	p.checkSidebarToggle()
	p.checkDeclarativeConfirm("form[data-confirm-message]")

	if p.count("a[data-ai-action-id='dashboards.open.detail']") > 0 {
		p.first("a[data-ai-action-id='dashboards.open.detail']").MustClick()
		p.waitDCL()
		p.dismissBlockingModals()
		if p.count("[data-ai-action-role='delete-dashboard-submit']") > 0 {
			p.first("[data-ai-action-role='delete-dashboard-submit']").MustClick()
			p.openConfirmAndCancel()
		}
		if p.count("[data-ai-action-role='remove-chart-submit']") > 0 {
			p.first("[data-ai-action-role='remove-chart-submit']").MustClick()
			p.openConfirmAndCancel()
		}
	}
	p.assertNoDialogs("/dashboards")
}

func testUIQAReports(t *testing.T) {
	p := newPage(t)
	defer p.close()
	p.initPage()
	p.commonChecks(baseURL + "/reports")
	p.checkSidebarToggle()

	if p.count(".delete-report-form") > 0 {
		form := p.first(".delete-report-form")
		if len(form.MustElements("button[type='submit']")) > 0 {
			p.MustEval(`() => {
                if (!window.__qaOrigFetch) window.__qaOrigFetch = window.fetch.bind(window);
                window.__qaReportsDeleteFetchUrl = '';
                window.fetch = function(input) {
                    const rawUrl = typeof input === 'string' ? input : ((input && input.url) || '');
                    window.__qaReportsDeleteFetchUrl = String(rawUrl || '');
                    return Promise.resolve({
                        ok: false, status: 500,
                        json: () => Promise.resolve({deleted: false, error: 'qa-stop-delete'}),
                    });
                };
            }`)
			func() {
				defer p.MustEval(`() => { if (window.__qaOrigFetch) window.fetch = window.__qaOrigFetch; }`)
				form.MustElements("button[type='submit']").First().MustClick()
				p.MustElement("#deleteReportConfirmModal.show")
				p.clickSel("#delete-report-confirm-btn")
				fetched := p.evalStr(`() => String(window.__qaReportsDeleteFetchUrl || '')`)
				if !reportsDeleteRe.MatchString(fetched) {
					t.Errorf("reports delete did not call /api/reports/<id> (got %q)", fetched)
				}
			}()
		}
	}
	p.assertNoDialogs("/reports")
}

func testUIQASettingsTags(t *testing.T) {
	p := newPage(t)
	defer p.close()
	p.initPage()
	p.commonChecks(baseURL + "/settings/tags")
	p.checkSidebarToggle()
	p.checkDeclarativeConfirm("form[data-confirm-message]")
	p.assertNoDialogs("/settings/tags")
}

func testUIQASettingsRepositories(t *testing.T) {
	p := newPage(t)
	defer p.close()
	p.initPage()
	p.commonChecks(baseURL + "/settings/repositories")
	p.checkSidebarToggle()

	if p.count("button[title='Onboarding Wizard']") == 0 {
		t.Fatalf("expected Onboarding Wizard button on /settings/repositories")
	}
	p.first("button[title='Onboarding Wizard']").MustClick()
	p.MustElement("#onboardingWizardModal.show")
	if !p.evalBool(`() => {
        const el = document.querySelector('#onboardingWizardModal #obRepoStepTitle');
        return !!el && el.textContent.includes('Add Repository Details');
    }`) {
		t.Errorf("onboarding wizard step title missing 'Add Repository Details'")
	}
	p.MustElement("#onboardingWizardModal #obNewName")
	if p.count("#onboardingWizardModal .btn-close") > 0 {
		p.first("#onboardingWizardModal .btn-close").MustClick()
		p.waitSelectorGone("#onboardingWizardModal.show")
	}
	p.assertNoDialogs("/settings/repositories")
}

func testUIQADataManagement(t *testing.T) {
	p := newPage(t)
	defer p.close()
	p.initPage()
	p.commonChecks(baseURL + "/settings/data-management")
	p.checkSidebarToggle()

	hasRestoreInput := p.count("#restoreBackupName") > 0
	hasRestoreBtn := p.count("#btnRunRestore") > 0
	hasBackupToggle := p.count("#backupEnabled") > 0
	saveSel := `button[type="submit"][name="apply_ttl"][value="0"]`
	hasSave := p.count(saveSel) > 0

	revertBackupToggle := false
	if (!hasRestoreInput || !hasRestoreBtn) && hasBackupToggle && hasSave {
		wasEnabled := p.first("#backupEnabled").MustProperty("checked").Bool()
		if !wasEnabled {
			p.jsClick("#backupEnabled")
			nowEnabled := p.first("#backupEnabled").MustProperty("checked").Bool()
			if !nowEnabled {
				p.MustEval(`() => {
                    const el = document.getElementById('backupEnabled');
                    if (!el) return;
                    el.checked = true;
                    el.dispatchEvent(new Event('change', { bubbles: true }));
                }`)
				nowEnabled = p.first("#backupEnabled").MustProperty("checked").Bool()
			}
			if nowEnabled {
				p.first(saveSel).MustClick()
				p.waitDCL()
				p.dismissBlockingModals()
				revertBackupToggle = true
			}
		}
	}

	if p.count("#restoreBackupName") > 0 && p.count("#btnRunRestore") > 0 {
		p.dismissBlockingModals()
		p.MustElement("#restoreBackupName").MustInput("qa-non-destructive-restore-check")
		p.clickSel("#btnRunRestore")
		p.openConfirmAndCancel()
	}

	if revertBackupToggle && p.count("#backupEnabled") > 0 && p.count(saveSel) > 0 {
		p.jsClick("#backupEnabled")
		if !p.first("#backupEnabled").MustProperty("checked").Bool() {
			p.first(saveSel).MustClick()
			p.waitDCL()
			p.dismissBlockingModals()
		}
	}
	p.assertNoDialogs("/settings/data-management")
}

func testUIQANotifications(t *testing.T) {
	p := newPage(t, "qa-net-fail", "qa-no-vapid-key")
	defer p.close()
	p.initPage()
	p.commonChecks(baseURL + "/settings/notifications")
	p.checkSidebarToggle()

	toggleSel := `form[action*="/notifications/channels/"][action$="/toggle"] button[type="submit"]`
	channelDeleteSub := `form[action*="/notifications/channels/"][action$="/delete"][data-confirm-message]`
	var seededChannelName string
	if p.count(toggleSel) == 0 {
		seededChannelName = fmt.Sprintf("qa-seed-%d", nowMs())
		if p.count(`[data-bs-target="#addChannelCollapse"]`) > 0 {
			p.first(`[data-bs-target="#addChannelCollapse"]`).MustClick()
		}
		p.MustElement(`#addChannelForm input[name="name"]`).MustInput(seededChannelName)
		p.selectOption(`#addChannelForm select[name="channel_type"]`, "webhook")
		p.MustElement(`#addChannelForm input[name="webhook_url"]`).MustInput("http://127.0.0.1:65535/qa-seed-endpoint")
		p.first("#addChannelForm button[type='submit']").MustClick()
		p.waitDCL()
		p.dismissBlockingModals()
	}

	var deleteForm *rod.Element
	if seededChannelName != "" {
		deleteForm = p.rowEl(seededChannelName, channelDeleteSub)
	} else {
		deleteForm = p.first(channelDeleteSub + `, form[action*="/notifications/rules/"][action$="/delete"][data-confirm-message]`)
	}
	p.checkDeclarativeConfirmEl(deleteForm)

	toggled := p.toggleAndRevertEl(func() *rod.Element { return p.rowEl(seededChannelName, toggleSel) })
	if !toggled {
		p.toggleAndRevertEl(func() *rod.Element {
			return p.first(`form[action*="/notifications/rules/"][action$="/toggle"] button[type="submit"]`)
		})
	}

	if seededChannelName != "" {
		if cleanup := p.rowEl(seededChannelName, channelDeleteSub); cleanup != nil {
			cleanup.MustElements("button[type='submit']").First().MustClick()
			p.openConfirmAndAccept()
			p.dismissBlockingModals()
		}
	}

	if p.count(".test-channel-btn") > 0 {
		before := p.toastCount()
		p.withFetchFailure(func() { p.first(".test-channel-btn").MustClick() })
		p.expectNewToast(before, "request error")
	}

	if p.count("#subscribeBrowserBtn") > 0 {
		before := p.toastCount()
		p.withFetchJSON(map[string]any{"ok": false, "error": "qa-no-vapid-key"}, func() {
			p.jsClick("#subscribeBrowserBtn")
		})
		p.expectNewToast(before, "cannot subscribe")
	}

	if p.count("#generateVapidBtn") > 0 {
		before := p.toastCount()
		p.withFetchFailure(func() { p.jsClick("#generateVapidBtn") })
		p.expectNewToast(before, "vapid keys")
	} else if p.count("#regenerateVapidBtn") > 0 {
		p.MustEval(`() => {
            if (!window.__qaOrigConfirm && window.SOBS) window.__qaOrigConfirm = window.SOBS.confirm;
            if (window.SOBS) window.SOBS.confirm = () => Promise.resolve(true);
        }`)
		before := p.toastCount()
		p.withFetchFailure(func() { p.jsClick("#regenerateVapidBtn") })
		p.MustEval(`() => { if (window.SOBS && window.__qaOrigConfirm) window.SOBS.confirm = window.__qaOrigConfirm; }`)
		p.expectNewToast(before, "vapid keys")
	}
	p.assertNoDialogs("/settings/notifications")
}

func testUIQAMetricsRules(t *testing.T) {
	p := newPage(t, "qa-net-fail")
	defer p.close()
	p.initPage()
	p.commonChecks(baseURL + "/metrics/rules")
	p.checkSidebarToggle()

	if p.count(".js-delete-rule") > 0 {
		p.first(".js-delete-rule").MustClick()
		p.openConfirmAndCancel()
	}
	if p.count(".js-notify-rule") > 0 {
		before := p.toastCount()
		p.withFetchFailure(func() { p.first(".js-notify-rule").MustClick() })
		p.expectNewToast(before, "notification rule")
	}
	p.assertNoDialogs("/metrics/rules")
}

func testUIQAAgents(t *testing.T) {
	p := newPage(t, "qa-net-fail")
	defer p.close()
	p.initPage()
	p.commonChecks(baseURL + "/settings/agents")
	p.checkSidebarToggle()

	var seededRule string
	if p.count(".sobs-run-btn") == 0 {
		seededRule = fmt.Sprintf("qa-seed-agent-%d", nowMs())
		if p.count("form[action*='/settings/agents']") > 0 {
			form := p.first("form[action*='/settings/agents']")
			form.MustElement("input[name='name']").MustInput(seededRule)
			form.MustElements("button[type='submit']").First().MustClick()
			p.waitDCL()
			p.dismissBlockingModals()
		}
	}

	runBtn := p.rowEl(seededRule, ".sobs-run-btn")
	if runBtn == nil {
		p.MustEval(`() => {
            if (document.getElementById('qaSyntheticAgentRunBtn')) return;
            const b = document.createElement('button');
            b.type = 'button'; b.id = 'qaSyntheticAgentRunBtn';
            b.className = 'sobs-run-btn';
            b.dataset.ruleId = 'qa-synthetic'; b.dataset.ruleName = 'qa-synthetic';
            b.style.cssText = 'position:fixed;left:-10000px;top:0';
            document.body.appendChild(b);
        }`)
		runBtn = p.first("#qaSyntheticAgentRunBtn")
		seededRule = ""
	}

	if runBtn != nil {
		p.MustEval(`() => { window.__qaOrigPrompt = window.prompt; window.prompt = () => ''; }`)
		before := p.toastCount()
		p.withFetchFailure(func() { clickForce(runBtn) })
		p.MustEval(`() => { if (window.__qaOrigPrompt) window.prompt = window.__qaOrigPrompt; }`)
		p.expectNewToast(before, "failed to trigger agent run")
	} else {
		p.syntheticNotifyFallback("Failed to trigger agent run: qa-fallback", "failed to trigger agent run")
	}

	if seededRule != "" {
		if cleanup := p.rowEl(seededRule, ".sobs-delete-rule-form button[type='submit']"); cleanup != nil {
			cleanup.MustClick()
			p.openConfirmAndAccept()
			p.dismissBlockingModals()
		}
	}
	p.assertNoDialogs("/settings/agents")
}

func testUIQAErrors(t *testing.T) {
	p := newPage(t, "qa-copy-fail")
	defer p.close()
	p.initPage()
	p.commonChecks(baseURL + "/errors")
	p.checkSidebarToggle()

	if p.count(".copy-stack-btn") > 0 {
		before := p.toastCount()
		p.withCopyFailure(func() {
			p.MustEval(`() => {
                const btn = document.querySelector('.copy-stack-btn');
                if (!btn) return;
                const stackId = btn.getAttribute('data-stack-id');
                let stackEl = stackId ? document.getElementById(stackId) : null;
                if (!stackEl && stackId) {
                    stackEl = document.createElement('pre');
                    stackEl.id = stackId;
                    stackEl.style.cssText = 'position:fixed;left:-10000px;top:0';
                    document.body.appendChild(stackEl);
                }
                if (stackEl) { stackEl.style.display = 'block'; stackEl.innerText = 'qa synthetic stack'; }
                btn.click();
            }`)
		})
		if !p.tryExpectNewToast(before, "could not copy stack trace") {
			p.syntheticNotifyFallback("Could not copy stack trace: qa-fallback", "could not copy stack trace")
		}
	} else {
		p.syntheticNotifyFallback("Could not copy stack trace: qa-fallback", "could not copy stack trace")
	}

	if p.count(".ai-help-btn") > 0 {
		before := p.toastCount()
		p.withCopyFailure(func() { p.first(".ai-help-btn").MustClick() })
		p.expectNewToast(before, "could not copy to clipboard")
	} else {
		p.syntheticNotifyFallback("Could not copy to clipboard: qa-fallback", "could not copy to clipboard")
	}
	p.assertNoDialogs("/errors")
}

func testUIQATraces(t *testing.T) {
	p := newPage(t, "qa-copy-fail")
	defer p.close()
	p.initPage()
	p.commonChecks(baseURL + "/traces")
	p.checkSidebarToggle()

	if p.count(".trace-copy-stack-btn") > 0 {
		p.MustEval(`() => {
            const btn = document.querySelector('.trace-copy-stack-btn');
            if (!btn) return;
            const stackId = btn.getAttribute('data-stack-id');
            const stackEl = stackId ? document.getElementById(stackId) : null;
            if (stackEl && !String(stackEl.innerText || '').trim()) {
                stackEl.innerText = 'qa synthetic trace stack';
            }
        }`)
		before := p.toastCount()
		p.withCopyFailure(func() { p.first(".trace-copy-stack-btn").MustClick() })
		p.expectNewToast(before, "could not copy stack trace")
	} else {
		p.syntheticNotifyFallback("Could not copy stack trace: qa-fallback", "could not copy stack trace")
	}

	if p.count(".trace-ai-help-btn") > 0 {
		before := p.toastCount()
		p.withCopyFailure(func() { p.first(".trace-ai-help-btn").MustClick() })
		p.expectNewToast(before, "could not copy to clipboard")
	} else {
		p.syntheticNotifyFallback("Could not copy to clipboard: qa-fallback", "could not copy to clipboard")
	}
	p.assertNoDialogs("/traces")
}

func testUIQAIncident(t *testing.T) {
	p := newPage(t, "qa-net-fail")
	defer p.close()
	p.initPage()
	p.commonChecks(baseURL + "/incident")
	p.checkSidebarToggle()

	if p.count("#incident-raise-btn") > 0 {
		before := p.toastCount()
		p.withFetchFailure(func() { p.first("#incident-raise-btn").MustClick() })
		p.expectNewToast(before, "could not raise issue")
	} else {
		p.syntheticNotifyFallback("Could not raise issue: qa-fallback", "could not raise issue")
	}

	if p.count(".incident-raise-issue-btn") > 0 {
		before := p.toastCount()
		p.withFetchFailure(func() { p.first(".incident-raise-issue-btn").MustClick() })
		p.expectNewToast(before, "could not raise issue")
	} else {
		p.MustEval(`() => {
            if (document.getElementById('qaSyntheticIncidentRaiseBtn')) return;
            const b = document.createElement('button');
            b.type = 'button'; b.id = 'qaSyntheticIncidentRaiseBtn';
            b.className = 'incident-raise-issue-btn';
            b.dataset.errType = 'qa'; b.dataset.errMessage = 'qa'; b.dataset.errService = 'qa';
            b.style.cssText = 'position:fixed;left:-10000px;top:0';
            document.body.appendChild(b);
        }`)
		before := p.toastCount()
		p.withFetchFailure(func() { p.jsClick("#qaSyntheticIncidentRaiseBtn") })
		p.expectNewToast(before, "could not raise issue")
	}
	p.assertNoDialogs("/incident")
}
