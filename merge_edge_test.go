package bridge

type edgeMergeFixture struct {
	BridgeURL, GiteaURL, Username, Password, Topic, TopicID string
	Targets                                                 []edgeMergeTarget
	RevertMergeSHA                                          string
}

type edgeMergeTarget struct {
	Repository string
	HeadSHA    string
	Blocked    bool
}

const mergeBrowserNavigation = edgeOAuth + `
const [edge, profile] = process.argv.slice(1);
const config = await readBrowserFixture();
const dashboard = config.BridgeURL + '/bridge/?' + new URLSearchParams({ topic_id: config.TopicID });
const summary = '已核验合并 2/3 仓，受阻 1 仓';

await withEdge(edge, profile, dashboard, [], async (evaluate, send, nextEvent) => {
  await authorize(evaluate, config.GiteaURL, config, dashboard, '#saved-topic', config.Topic);
  const enabled = await send('Page.enable');
  if (enabled.error) throw new Error('could not observe browser confirmation dialogs');
  const wait = (label, fn, ...args) => waitForPage(label, () => evaluate(fn, ...args));
  async function confirmOperation(repository, label, expectedSHA) {
    const opened = nextEvent('Page.javascriptDialogOpening');
    await Promise.all([
      evaluate((url, name, action) => {
        if (location.href !== url) throw new Error('unexpected Bridge operation page');
        const card = Array.from(document.querySelectorAll('#results article')).find(
          item => item.querySelector('h2')?.textContent === 'labowner/' + name);
        const button = Array.from(card?.querySelectorAll('button') ?? []).find(
          item => item.textContent === action && !item.disabled);
        if (!button) throw new Error('expected operation button was unavailable');
        setTimeout(() => button.click(), 0);
      }, dashboard, repository, label),
      opened.then(async dialog => {
        if (dialog.type !== 'confirm' || dialog.url !== dashboard || !dialog.message.includes(expectedSHA)) {
          throw new Error('confirmation did not identify the expected operation commit');
        }
        const reply = await send('Page.handleJavaScriptDialog', { accept: true });
        if (reply.error) throw new Error('could not confirm the Bridge operation');
      })
    ]);
  }

  for (const target of config.Targets ?? []) {
    await confirmOperation(target.Repository, '确认合并这一仓', target.HeadSHA);
    await wait('per-repository merge result for ' + target.Repository, (name, blocked) => {
      const card = Array.from(document.querySelectorAll('#results article')).find(
        item => item.querySelector('h2')?.textContent === 'labowner/' + name);
      if (!card) return false;
      return blocked ? card.dataset.merge === 'blocked' && card.innerText.includes('合并受阻：PR 未就绪')
        : card.innerText.includes('已核验合并 ·');
    }, target.Repository, target.Blocked);
  }
  await wait('summary of two merged and one blocked repository', text =>
    document.querySelector('#saved-topic')?.textContent.includes(text), summary);

  if (config.RevertMergeSHA) {
    await confirmOperation('no-match', '确认生成反向 PR', config.RevertMergeSHA);
    const approvalURL = await wait('reverse PR link with Gitea approval label', base => {
      const card = Array.from(document.querySelectorAll('#results article')).find(
        item => item.querySelector('h2')?.textContent === 'labowner/no-match');
      return Array.from(card?.querySelectorAll('a') ?? []).find(
        link => link.href.startsWith(base + '/labowner/no-match/pulls/') &&
          /^反向 PR #[0-9]+（到 Gitea 审批）$/.test(link.textContent))?.href;
    }, config.GiteaURL);
    await evaluate(url => Array.from(document.querySelectorAll('#results a')).find(
      link => link.href === url && link.textContent.includes('到 Gitea 审批')).click(), approvalURL);
    await wait('reverse PR page in Gitea', url =>
      location.href === url && document.title.includes('Revert merged change'), approvalURL);
  }
});
`
