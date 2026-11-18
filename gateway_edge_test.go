package bridge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type edgeGatewayFixture struct {
	Gateway, Password, Topic, TopicID, ReceiptID string
	Anchored                                     bool
}

func TestSCMHostedStackEdge(t *testing.T) {
	fixture, path := loadSCMFixture(t)
	evidence := loadSCMEvidence(t, path)
	verifyGatewayInEdge(t, fixture.browser(t), edgeGatewayFixture{
		Gateway: fixture.Gateway, Password: fixture.Password, Topic: "CAD-SCM", TopicID: evidence.TopicID,
		ReceiptID: strings.TrimPrefix(evidence.Receipt.Path, "/bridge/api/receipts/"), Anchored: true,
	})
}

func verifyGatewayInEdge(t *testing.T, trustedClient *http.Client, fixture edgeGatewayFixture) {
	t.Helper()
	edge := edgeForTest(t)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("the live browser test requires Node.js with built-in WebSocket")
	}
	gateway, err := url.Parse(fixture.Gateway)
	if err != nil || gateway.Scheme != "https" || gateway.User != nil ||
		(gateway.Hostname() != "127.0.0.1" && gateway.Hostname() != "git.office.example") {
		t.Fatal("the Edge login fixture must use an isolated HTTPS gateway")
	}
	response, err := trustedClient.Get(fixture.Gateway + "/")
	if err != nil {
		t.Fatalf("verify the laboratory gateway before opening Edge: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK || response.TLS == nil || len(response.TLS.VerifiedChains) == 0 {
		t.Fatal("the laboratory gateway did not present a verified certificate")
	}
	// Permit only the leaf key already authenticated against the temporary CA.
	// This scopes the Chromium exception to this fixture's Edge process.
	pin := sha256.Sum256(response.TLS.PeerCertificates[0].RawSubjectPublicKeyInfo)
	payload, err := json.Marshal(struct {
		edgeGatewayFixture
		Pin string
	}{fixture, base64.StdEncoding.EncodeToString(pin[:])})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	browser := exec.CommandContext(ctx, node, "--input-type=module", "-e", gatewayBrowserNavigation, edge, t.TempDir())
	browser.Stdin = bytes.NewReader(payload)
	browser.WaitDelay = 2 * time.Second
	if output, err := browser.CombinedOutput(); err != nil {
		t.Fatalf("live Edge navigation failed: %v\n%s", err, output)
	}
	t.Log("Edge followed both Gitea menus, completed real OAuth, rendered the original saved topic and project overview, and reopened the same receipt after a fresh sign-in.")
}

const gatewayBrowserNavigation = edgeDevTools + `
const [edge, profile] = process.argv.slice(1);
let input = '';
for await (const part of process.stdin.setEncoding('utf8')) input += part;
const config = JSON.parse(input);
const { Gateway: origin, TopicID: topicID, Topic: topic, ReceiptID: receiptID } = config;
const hostname = new URL(origin).hostname;
const flags = ['--ignore-certificate-errors-spki-list=' + config.Pin];
if (hostname !== '127.0.0.1') flags.push('--host-resolver-rules=MAP ' + hostname + ' 127.0.0.1');
const dashboard = origin + '/bridge/?' + new URLSearchParams({ topic_id: topicID });
const receiptURL = origin + '/bridge/receipts/?' + new URLSearchParams({ id: receiptID, topic_id: topicID });

await withEdge(edge, profile, origin + '/', flags, async (evaluate, send) => {
  const wait = (label, fn, ...args) => waitForPage(label, () => evaluate(fn, ...args));
  async function goTo(url) {
    const reply = await send('Page.navigate', { url });
    if (reply.error || reply.result?.errorText) throw new Error('navigation to the fixture failed');
  }
  async function openMenu(path) {
    await wait('Gitea menu link ' + path, (site, route) =>
      location.origin === site && location.pathname === '/' &&
      !!document.querySelector('a[href="/bridge/"]') &&
      !!document.querySelector('a[href="/bridge/receipts/"]') &&
      !!document.querySelector('a[href="' + route + '"]'), origin, path);
    await evaluate(route => document.querySelector('a[href="' + route + '"]').click(), path);
  }

  async function authorize(returnURL, selector) {
    const state = () => evaluate((site, target, field, number) => {
      if (location.origin !== site) return false;
      if (location.href === target && document.querySelector(field)?.innerText.includes(number)) return 'returned';
      if (document.querySelector('form[action="/login/oauth/grant"]')) return 'consent';
      if (location.pathname === '/user/login' && document.querySelector('input[name="user_name"]')?.form) return 'credentials';
      return false;
    }, origin, returnURL, selector, topic);
    await wait('Bridge login link', url => location.href === url && !!document.querySelector('#login:not([hidden])'), returnURL);
    await evaluate(() => document.querySelector('#login').click());
    let step = await waitForPage('Gitea OAuth flow', state);
    if (step === 'credentials') {
      await evaluate((site, password) => {
        if (location.origin !== site || location.pathname !== '/user/login') throw new Error('unexpected login origin');
        const user = document.querySelector('input[name="user_name"]');
        if (new URL(user.form.action, location.href).origin !== site) throw new Error('unexpected login submission');
        user.value = 'labreader';
        user.form.querySelector('input[name="password"]').value = password;
        user.form.requestSubmit();
      }, origin, config.Password);
      step = await waitForPage('Gitea login result', async () => {
        const result = await state();
        return result !== 'credentials' && result;
      });
    }
    if (step === 'consent') {
      await evaluate(site => {
        if (location.origin !== site) throw new Error('unexpected consent origin');
        document.querySelector('form[action="/login/oauth/grant"] [name="granted"][value="true"]').click();
      }, origin);
    }
    await waitForPage('OAuth return to the original URL and data', async () => await state() === 'returned');
  }

  await openMenu('/bridge/');
  await wait('anonymous dashboard after menu click', site =>
    location.href === site + '/bridge/' && !!document.querySelector('#login:not([hidden])'), origin);
  await goTo(dashboard);
  await authorize(dashboard, '#saved-topic');
  await wait('saved topic repository', () => document.querySelector('#results h2')?.textContent === 'labowner/fixture');
  await wait('authenticated dashboard', () => !!document.querySelector('#overview:not([hidden])'));
  await evaluate(() => document.querySelector('#overview summary').click());
  await wait('live project overview with canonical commit link', site =>
    Array.from(document.querySelectorAll('#overview-repos a')).some(link =>
      link.href.startsWith(site + '/labowner/fixture/commit/')), origin);

  await goTo(origin + '/');
  await openMenu('/bridge/receipts/');
  await wait('receipt center after Gitea menu click', site =>
    location.href === site + '/bridge/receipts/' && !!document.querySelector('#find input[name="id"]'), origin);
  await evaluate(id => {
    const input = document.querySelector('#find input[name="id"]');
    input.value = id;
    input.form.requestSubmit();
  }, receiptID);
  const details = (number, id, anchored) => {
    const article = document.querySelector('#receipt article');
    if (!article || article.querySelector('h2')?.textContent !== number + ' · v1' ||
        !article.innerText.includes('artifact.txt')) return false;
    const endpoint = '/bridge/api/receipts/' + id;
    const has = format => !!article.querySelector('a[href="' + endpoint + '?format=' + format + '"]');
    return has('json') && has('markdown') &&
      (!anchored || (has('proof') && article.innerText.includes('独立存证已核验')));
  };
  await wait('rendered receipt and export links', details, topic, receiptID, config.Anchored);
  await wait('same receipt and topic in version history', url =>
    Array.from(document.querySelectorAll('#history a')).some(link => link.href === url), receiptURL);

  await send('Network.clearBrowserCookies');
  await goTo(receiptURL);
  await authorize(receiptURL, '#receipt');
  await wait('same receipt after fresh OAuth', details, topic, receiptID, config.Anchored);
});
`
