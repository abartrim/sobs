package integration

import (
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

var reportsDeleteRe = regexp.MustCompile(`/api/reports/.+`)

// rowEl returns the first element matching sub, optionally scoped to the table
// row whose text contains rowText (replacement for Playwright's
// `tr:has-text('X') sub`). rowText == "" means page-wide first match.
func (p *page) rowEl(rowText, sub string) *rod.Element {
	if rowText == "" {
		return p.first(sub)
	}
	for _, r := range p.MustElements("tr") {
		if strings.Contains(r.MustText(), rowText) {
			if els := r.MustElements(sub); len(els) > 0 {
				return els.First()
			}
		}
	}
	return nil
}

// checkDeclarativeConfirmEl submits a declarative-confirm form element (or
// skips if nil) and cancels the resulting confirm modal.
func (p *page) checkDeclarativeConfirmEl(form *rod.Element) bool {
	if form == nil {
		return false
	}
	if btns := form.MustElements("button[type='submit'],input[type='submit']"); len(btns) > 0 {
		btns.First().MustClick()
	} else {
		form.MustEval(`n => (typeof n.requestSubmit === 'function' ? n.requestSubmit() : n.submit())`)
	}
	p.openConfirmAndCancel()
	return true
}

// toggleAndRevertEl clicks a toggle button, waits for reload, then clicks it
// again to revert. getBtn is re-queried after the reload. Returns false if the
// button is absent at either step.
func (p *page) toggleAndRevertEl(getBtn func() *rod.Element) bool {
	btn := getBtn()
	if btn == nil {
		return false
	}
	btn.MustScrollIntoView()
	btn.MustClick()
	p.waitDCL()
	btn2 := getBtn()
	if btn2 == nil {
		return false
	}
	btn2.MustClick()
	p.waitDCL()
	return true
}

// clickForce clicks an element, falling back to a DOM .click() when the
// element is not interactable (e.g. an off-screen synthetic button).
func clickForce(el *rod.Element) {
	defer func() {
		if recover() != nil {
			el.MustEval(`() => this.click()`)
		}
	}()
	el.MustClick()
}

// tryExpectNewToast is the non-fatal variant of expectNewToast.
func (p *page) tryExpectNewToast(before int, hint string) bool {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ok := p.evalBool(`({ count, hint }) => {
            const c = document.getElementById('sobsNotifyToastContainer');
            if (!c) return false;
            const all = Array.from(c.querySelectorAll('.toast'));
            if (all.length <= count) return false;
            if (!hint) return true;
            return all.slice(count).some(el =>
                String(el.textContent || '').toLowerCase().includes(hint.toLowerCase())
            );
        }`, map[string]any{"count": before, "hint": hint})
		if ok {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
