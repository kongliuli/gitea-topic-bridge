const status = document.querySelector('#message');
const view = document.querySelector('#receipt');
const history = document.querySelector('#history');
const issue = document.querySelector('#issue');
const find = document.querySelector('#find');
const login = document.querySelector('#login');
const current = new URLSearchParams(location.search);
const validID = (value) => /^[A-Za-z0-9_-]{43}$/.test(value ?? '');
const date = (value) => new Date(value).toLocaleString();
let submission;

function element(tag, value) {
  const node = document.createElement(tag);
  node.textContent = value;
  return node;
}

function link(label, target) {
  const node = element('a', label);
  node.href = target;
  return node;
}

function page(id, topicID) {
  const query = new URLSearchParams();
  if (id) query.set('id', id);
  if (topicID) query.set('topic_id', topicID);
  return `/bridge/receipts/?${query}`;
}

function errorStatus(response) {
  if (response.status === 401) {
    login.href = `/bridge/auth/login?${new URLSearchParams({ page: 'receipts', id: current.get('id') ?? '', topic_id: current.get('topic_id') ?? '' })}`;
    login.hidden = false;
    issue.hidden = true;
    view.replaceChildren();
    history.replaceChildren();
    status.textContent = '请先用 Gitea 登录。';
    return;
  }
  if (response.status === 404) {
    issue.hidden = true;
    history.replaceChildren();
  }
  status.textContent = response.status === 404 ? '找不到这条记录，或没有全部仓库的查看权限。' : '暂时无法读取，请稍后重试。';
}

function renderReceipt(data, anchored) {
  const snapshot = data.snapshot;
  const endpoint = `/bridge/api/receipts/${snapshot.id}`;
  const box = document.createElement('article');
  const anchorText = {
    verified: '独立存证已核验',
    pending: '已签名 · 独立存证待确认',
    not_configured: '已签名 · 未配置独立存证',
    check_unavailable: '已签名 · 暂不能验证独立存证',
    proof_invalid: '已签名 · 独立凭证未通过验证',
  };
  box.append(element('h2', `${snapshot.topic} · v${snapshot.version}`),
    element('p', `${date(snapshot.issued_at)} · 签发账号 #${snapshot.issuer_id}`),
    element('p', anchorText[anchored] ?? '独立存证状态未知'),
    element('p', `SHA-256 ${data.sha256}`));
  const downloads = document.createElement('p');
  downloads.append(link('下载签名 JSON', `${endpoint}?format=json`), ' · ', link('下载 Markdown', `${endpoint}?format=markdown`));
  if (anchored === 'verified') downloads.append(' · ', link('下载独立凭证', `${endpoint}?format=proof`));
  if (snapshot.previous_id) downloads.append(' · ', link('查看更正前版本', page(snapshot.previous_id, snapshot.topic_id)));
  box.append(downloads);
  for (const member of snapshot.members) {
    const source = member.source;
    const group = document.createElement('section');
    group.append(element('h3', `${member.repository.owner}/${member.repository.name}`));
    if (source) {
      group.append(element('p', `${source.tag ? `发布 ${source.tag}` : `合并 PR #${source.object_id}`} · ${source.commit}`));
    } else {
      group.append(element('p', '未核验源提交'));
    }
    const paths = member.changes?.paths;
    if (paths) {
      const detail = document.createElement('details');
      detail.append(element('summary', `变更清单（${paths.length} 项）`), element('pre', paths.join('\n')));
      group.append(detail);
    } else group.append(element('p', '未核验变更清单'));
    box.append(group);
  }
  box.append(element('h3', '制品快照'));
  if (!snapshot.artifacts.length) box.append(element('p', '未提供制品'));
  for (const artifact of snapshot.artifacts) {
    box.append(element('p', `仓库 ${artifact.repository_id} · ${artifact.path} · ${artifact.size} 字节 · ${artifact.sha256}`));
  }
  box.append(element('p', '未核验构建来源或测试结果。请用通过独立渠道取得的公钥核验 JSON 与凭证。'));
  view.replaceChildren(box);
}

function field(group, label, placeholder, max) {
  const wrapper = element('label', label);
  const input = document.createElement('input');
  input.placeholder = placeholder;
  input.maxLength = max;
  wrapper.append(input);
  group.append(wrapper);
  return input;
}

function configureIssue(data, topicID) {
  if (!data.can_issue || data.versions.length === 0 && data.before) return;
  const newest = data.versions[0];
  const node = document.querySelector('#members');
  node.replaceChildren();
  const inputs = data.repositories.map((repo) => {
    const group = document.createElement('fieldset');
    group.append(element('legend', `${repo.owner}/${repo.name}`));
    node.append(group);
    const artifact = field(group, '制品相对路径（可选）', 'dist/bundle.zip', 300);
    const release = field(group, '发布数字 ID（可选）', '7', 15);
    const sha = field(group, '发布对应的完整提交 SHA', '40 或 64 位十六进制', 64);
    return { repo, artifact, release, sha };
  });
  issue.hidden = false;
  document.querySelector('#issue-title').textContent = newest ? `更正 v${newest.version}（旧版保留）` : '签发首版';
  issue.addEventListener('submit', async (event) => {
    event.preventDefault();
    const artifacts = [];
    const sources = [];
    for (const item of inputs) {
      const repoID = item.repo.id;
      const artifact = item.artifact.value.trim();
      const release = item.release.value.trim();
      const sha = item.sha.value.trim().toLowerCase();
      if (artifact) artifacts.push({ repository_id: repoID, path: artifact });
      if (release || sha) {
        if (!/^[1-9][0-9]{0,14}$/.test(release) || !/^(?:[a-f0-9]{40}|[a-f0-9]{64})$/.test(sha)) {
          status.textContent = '请同时填写发布数字 ID 和完整提交 SHA。';
          return;
        }
        sources.push({ repository_id: repoID, release_id: Number(release), expected_sha: sha });
      }
    }
    if (artifacts.length > 8 || sources.length > 16) {
      status.textContent = '一张回执最多提供 8 个制品和 16 项发布来源。';
      return;
    }
    if (!window.confirm(newest ? '签发更正版并保留旧版？' : '签发这张回执？未核验的内容会明确标注。')) return;
    const body = JSON.stringify({ topic_id: topicID, previous_id: newest?.id ?? '', artifacts, sources });
    if (submission?.body !== body) submission = { body, key: crypto.randomUUID() };
    const submit = issue.querySelector('button');
    submit.disabled = true;
    status.textContent = '正在核验并签发…';
    try {
      const response = await fetch('/bridge/api/receipts', { method: 'POST',
        headers: { 'Content-Type': 'application/json', 'Idempotency-Key': submission.key }, body });
      if (response.status === 409) {
        status.textContent = '回执版本或发布提交发生变化，请刷新后核对。';
        return;
      }
      if (response.status === 422) {
        status.textContent = '有制品无法读取或不符合安全限制。';
        return;
      }
      if (!response.ok) { errorStatus(response); return; }
      location.assign(page((await response.json()).snapshot.id, topicID));
    } catch {
      status.textContent = '没有收到签发确认。保持输入不变可重试。';
    } finally {
      submit.disabled = false;
    }
  });
}

async function loadHistory(topicID, before = '', list) {
  const query = new URLSearchParams({ topic_id: topicID });
  if (before) query.set('before', before);
  const response = await fetch(`/bridge/api/receipts?${query}`);
  if (!response.ok) { errorStatus(response); return; }
  const data = await response.json();
  if (!list) {
    list = document.createElement('ul');
    history.replaceChildren(element('h2', `${data.topic} · 版本历史`), list);
    configureIssue(data, topicID);
  }
  for (const version of data.versions) {
    const row = document.createElement('li');
    row.append(link(`v${version.version} · ${date(version.issued_at)}`, page(version.id, topicID)));
    list.append(row);
  }
  if (data.before) {
    const more = element('button', '读取更早版本');
    more.addEventListener('click', async () => {
      more.disabled = true;
      try { await loadHistory(topicID, data.before, list); more.remove(); }
      catch { status.textContent = '暂时无法读取，请稍后重试。'; more.disabled = false; }
    });
    history.append(more);
  } else if (!list.children.length) list.append(element('li', '暂无可查看的回执'));
}

find.addEventListener('submit', (event) => {
  event.preventDefault();
  if (validID(find.elements.id.value.trim())) location.assign(page(find.elements.id.value.trim()));
});

async function start() {
  try {
    const id = current.get('id');
    let topicID = current.get('topic_id');
    if (!validID(id) && !validID(topicID)) {
      status.textContent = '输入回执 ID，或从已保存的联调单打开回执中心。';
      return;
    }
    status.textContent = '正在读取…';
    if (validID(id)) {
      find.elements.id.value = id;
      const response = await fetch(`/bridge/api/receipts/${id}`);
      if (!response.ok) { errorStatus(response); return; }
      const data = await response.json();
      renderReceipt(data, response.headers.get('X-Bridge-Anchor-Status'));
      topicID = data.snapshot.topic_id;
    }
    document.querySelector('#dashboard').href = `/bridge/?${new URLSearchParams({ topic_id: topicID })}`;
    status.textContent = '';
    await loadHistory(topicID);
  } catch {
    status.textContent = '暂时无法读取，请稍后重试。';
  }
}

start();
