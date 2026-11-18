const form = document.querySelector('#lookup');
const login = document.querySelector('#login');
const logout = document.querySelector('#logout');
const account = document.querySelector('#account-name');
const message = document.querySelector('#message');
const results = document.querySelector('#results');
const save = document.querySelector('#save-topic');
const savedTopic = document.querySelector('#saved-topic');
const createPulls = document.querySelector('#create-pulls');
const submit = form.querySelector('button');
const project = form.elements.namedItem('project_id');
const topic = form.elements.namedItem('topic');
const statuses = { complete: '已查完', incomplete: '未查完', failed: '查询失败' };
const lifecycles = { merged: '已合并', open: '开放', closed: '关闭未合并' };
const checks = { success: '已报告的检查通过', pending: '等待中', warning: '警告', failure: '失败', error: '出错', skipped: '跳过', not_reported: '未报告' };
const conflicts = { none_reported: '未报告冲突', not_mergeable_reason_unknown: 'Gitea 暂不可合并，原因未知', not_applicable: '不适用' };
let activeQuery;
let storeEnabled = false;
let writesEnabled = false;
let mergeEnabled = false;
let revertEnabled = false;
let draft;
let savedBatch;
let mergeBusy = false;

function element(tag, text) {
  const node = document.createElement(tag);
  node.textContent = text;
  return node;
}

function requireLogin() {
  activeQuery?.abort();
  login.href = `/bridge/auth/login${location.search}`;
  login.hidden = false;
  logout.hidden = true;
  form.hidden = true;
  account.textContent = '';
  clearSavedTopic();
  results.replaceChildren();
  message.textContent = '先登录 Gitea，再查看你有权限的仓库。';
}

function showResults(data, decorate) {
  results.replaceChildren();
  if (!data.complete) {
    message.textContent = '查询未完成。下面只列出已读到的结果。';
  } else if (!data.found) {
    message.textContent = '在可见范围内未找到匹配的合并请求。';
  } else {
    message.textContent = `可见仓库已查完 · ${new Date(data.read_at).toLocaleString()}`;
  }
  for (const result of data.repositories) {
    const card = element('article', '');
    const repo = result.repository;
    card.append(element('h2', `${repo.owner}/${repo.name}`), element('p', statuses[result.status] ?? '未知'));
    if (result.pull_requests.length === 0 && result.status === 'complete') {
      card.append(element('p', '本仓无匹配。'));
    }
    for (const pull of result.pull_requests) {
      const line = element('div', '');
      line.className = 'pull';
      const link = element('a', `#${pull.number} ${pull.title}`);
      link.href = pull.url;
      line.append(link, element('p', `${lifecycles[pull.lifecycle] ?? '未知'} · head ${pull.head_sha || '未知'}`));
      const reviews = pull.reviews ? `批准 ${pull.reviews.approved}，要求修改 ${pull.reviews.changes_requested}（当前提交，不代表满足合并规则）` : '未知';
      line.append(element('p', `CI：${checks[pull.ci] ?? '未知'} · 冲突：${conflicts[pull.conflict] ?? '未知'} · 变更文件数：${pull.changed_files ?? '未知'}`));
      line.append(element('p', `正式评审：${reviews} · 读取于 ${new Date(pull.read_at).toLocaleString()}`));
      card.append(line);
    }
    decorate?.(card, repo);
    results.append(card);
  }
}

function inputRow(parent, name, type, value = '') {
  const label = element('label', name);
  const control = document.createElement('input');
  control.type = type;
  control.value = value;
  if (type === 'text') control.maxLength = 200;
  label.append(control);
  parent.append(label);
  return control;
}

async function lookup() {
  activeQuery?.abort();
  const query = new AbortController();
  activeQuery = query;
  const params = new URLSearchParams(new FormData(form));
  history.replaceState(null, '', `/bridge/?${params}`);
  clearSavedTopic();
  submit.disabled = true;
  message.textContent = '正在查找…';
  results.replaceChildren();
  try {
    const response = await fetch(`/bridge/api/topics/lookup?${params}`, { signal: query.signal });
    if (response.status === 401) return requireLogin();
    if (response.status === 404) {
      message.textContent = '找不到这个项目组，或你没有查看权限。';
      return;
    }
    if (!response.ok) throw new Error('lookup failed');
    const data = await response.json();
    if (!query.signal.aborted) {
      const picked = [];
      showResults(data, (card, repo) => {
        if (!writesEnabled) return;
        const choices = element('fieldset', '');
        choices.append(element('legend', '保存联调单时使用'));
        const included = inputRow(choices, '纳入此仓', 'checkbox');
        included.checked = true;
        const source = inputRow(choices, '源分支（可留空）', 'text');
        const target = inputRow(choices, '目标分支（可留空）', 'text');
        picked.push({ repo, included, source, target });
        card.append(choices);
      });
      if (storeEnabled && data.repositories.length) {
        draft = {
          params,
          picked,
          repos: data.repositories.map((item) => ({ repository_id: item.repository.id })),
        };
        save.disabled = false;
        save.hidden = false;
      }
    }
  } catch {
    if (!query.signal.aborted) message.textContent = '暂时查不到结果，请稍后重试。';
  } finally {
    if (activeQuery === query) submit.disabled = false;
  }
}

function clearSavedTopic() {
  draft = undefined;
  savedBatch = undefined;
  save.hidden = true;
  createPulls.hidden = true;
  savedTopic.hidden = true;
  savedTopic.replaceChildren();
}

save.addEventListener('click', async () => {
  const submitted = draft;
  if (!submitted) return;
  const members = writesEnabled ? submitted.picked.filter((item) => item.included.checked).map((item) => ({
    repository_id: item.repo.id,
    source_branch: item.source.value.trim(),
    target_branch: item.target.value.trim(),
  })) : submitted.repos;
  savedTopic.hidden = false;
  if (!members.length || members.some((member) => Boolean(member.source_branch) !== Boolean(member.target_branch))) {
    savedTopic.textContent = '至少选一个仓库。源分支和目标分支请同时填写，或同时留空。';
    return;
  }
  const body = JSON.stringify({ project_id: submitted.params.get('project_id'), topic: submitted.params.get('topic'), members });
  if (submitted.body !== body) {
    submitted.body = body;
    submitted.key = crypto.randomUUID();
  }
  save.disabled = true;
  savedTopic.textContent = '正在保存…';
  try {
    const response = await fetch('/bridge/api/topics', { method: 'POST', headers: { 'Content-Type': 'application/json', 'Idempotency-Key': submitted.key }, body: submitted.body });
    if (draft !== submitted) return;
    if (response.status === 401) return requireLogin();
    if (response.status === 404) {
      savedTopic.textContent = '仓库权限可能已变化，请重新查询后再保存。';
      return;
    }
    if (response.status === 400) {
      savedTopic.textContent = '请检查联调单输入。源分支与目标分支不能相同，分支名不能含空格。';
      return;
    }
    if (!response.ok) throw new Error('save failed');
    const data = await response.json();
    if (draft !== submitted) return;
    const link = element('a', `打开联调单 ${data.topic} · v${data.version}`);
    link.href = `/bridge/?${new URLSearchParams({ topic_id: data.id })}`;
    savedTopic.replaceChildren(link);
    draft = undefined;
    save.hidden = true;
  } catch {
    if (draft === submitted) savedTopic.textContent = '没收到保存确认，可以重试，不会重复建单。';
  } finally {
    if (draft === submitted) save.disabled = false;
  }
});

async function openSavedTopic(id, note = '') {
  clearSavedTopic();
  activeQuery?.abort();
  const query = new AbortController();
  activeQuery = query;
  results.replaceChildren();
  message.textContent = '正在读取联调单…';
  try {
    const response = await fetch(`/bridge/api/topics/${encodeURIComponent(id)}`, { signal: query.signal });
    if (response.status === 401) return requireLogin();
    if (response.status === 404) {
      message.textContent = '找不到这个联调单，或你没有查看全部成员仓的权限。';
      return;
    }
    if (!response.ok) throw new Error('saved topic failed');
    const data = await response.json();
    if (query.signal.aborted) return;
    const repos = data.repositories;
    const memberByRepo = new Map(data.members.map((member) => [member.repository_id, member]));
    const selected = [];
    const operationStates = { pending: '待提交', linked: '已关联 PR', failed: '提交失败', unconfirmed: '结果待核对（重试只回查）' };
    const reasons = { created: '已新建', existing: '复用已有 PR', branches_in_use: '该分支组合已有其他 PR', scan_incomplete: '没有查完现有 PR', precheck_failed: '提交前检查失败', needs_reconciliation: 'Gitea 尚未确认', gitea_rejected: 'Gitea 拒绝，请检查分支和改动', write_denied: 'Gitea 未允许写入', login_required: '需要重新登录' };
    showResults({ repositories: repos, complete: repos.every((item) => item.status === 'complete'), found: repos.some((item) => item.pull_requests.length), read_at: data.read_at }, (card, repo) => {
      const member = memberByRepo.get(repo.id);
      if (!member?.source_branch || !member.target_branch) return;
      const status = [operationStates[member.state] ?? '未知', reasons[member.result_code]].filter(Boolean).join(' · ');
      card.append(element('p', `${member.source_branch} → ${member.target_branch} · ${status}`));
      if (member.pull_number) {
        const repoResult = repos.find((item) => item.repository.id === repo.id);
        const pull = repoResult.pull_requests.find((item) => item.number === member.pull_number);
        renderMerge(card, id, member, pull);
        renderRevert(card, id, member, repoResult.pull_requests);
      } else if (writesEnabled) {
        const include = inputRow(card, member.state === 'unconfirmed' ? '仅核对此仓的 PR' : '选中此仓建 PR', 'checkbox');
        selected.push({ include, member });
      }
    });
    savedBatch = { id, selected };
    createPulls.disabled = false;
    createPulls.hidden = !selected.length;
    savedTopic.hidden = false;
    const merged = data.members.filter((member) => member.merge?.state === 'merged').length;
    const blocked = data.members.filter((member) => member.merge?.state === 'blocked').length;
    const counts = mergeEnabled || merged || blocked ? ` · 已核验合并 ${merged}/${data.members.length} 仓，受阻 ${blocked} 仓` : '';
    savedTopic.textContent = `联调单 ${data.topic} · v${data.version} · 已关联 ${data.members.filter((member) => member.pull_number).length}/${data.members.length} 仓${counts}${note ? ' · ' + note : ''}`;
    topic.value = data.topic;
    project.value = data.project_id;
  } catch {
    if (!query.signal.aborted) message.textContent = '暂时读不到保存的联调单。';
  }
}

function renderMerge(card, topicID, member, pull) {
  card.append(element('p', `已关联 #${member.pull_number}`));
  const status = member.merge;
  const mergeReasons = {
    pull_not_ready: 'PR 未就绪（可能是冲突、后台检查中或已关闭）',
    branch_policy_unknown: '拿不到目标分支保护',
    branch_policy_unsafe: '目标分支未满足保护和审批要求，或当前账号可直接推送',
    approval_missing: '当前提交的正式批准不足，或仍有修改要求',
    gitea_rejected_merge: 'Gitea 拒绝合并，请在原 PR 查看规则和检查项',
  };
  if (status?.state === 'merged') {
    card.append(element('p', `已核验合并 · ${status.merge_sha}`));
    return;
  }
  if (status?.state === 'blocked') {
    card.dataset.merge = 'blocked';
    card.append(element('p', `合并受阻：${mergeReasons[status.result_code] ?? '请检查原 PR'}`));
  }
  const pending = status?.state === 'unconfirmed';
  if (pending) card.append(element('p', '合并结果待核对；不会自动重新提交。'));
  if (!mergeEnabled) return;
  const reconcile = pending || pull?.lifecycle === 'merged';
  if (!reconcile && (pull?.lifecycle !== 'open' || !/^(?:[0-9a-f]{40}|[0-9a-f]{64})$/.test(pull.head_sha))) return;
  const button = element('button', reconcile ? '核对合并结果' : '确认合并这一仓');
  button.type = 'button';
  button.addEventListener('click', async () => {
    if (mergeBusy || savedBatch?.id !== topicID) return;
    if (!reconcile && !window.confirm(`确认合并 #${pull.number} 到 ${member.target_branch}？\n提交：${pull.head_sha}\n只操作这一仓，不会绕过 Gitea 检查。`)) return;
    const current = savedBatch;
    mergeBusy = true;
    button.disabled = true;
    message.textContent = reconcile ? '正在核对 Gitea…' : '正在提交合并请求…';
    let note = '已刷新合并结果';
    try {
      const response = await fetch(`/bridge/api/topics/${encodeURIComponent(topicID)}/members/${member.id}/merge`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(reconcile ? { reconcile: true } : { confirm: true, expected_head: pull.head_sha }),
      });
      if (savedBatch !== current) return;
      if (response.status === 401) return requireLogin();
      if (response.status === 403) note = '当前账号不能合并这一仓';
      else if (response.status === 409) note = '关联 PR 或提交已变化，请检查后重新确认';
      else if (!response.ok) note = '未收到合并确认，先核对已保存的状态';
    } catch {
      note = '未收到合并确认，先核对已保存的状态';
    } finally {
      mergeBusy = false;
      button.disabled = false;
    }
    if (savedBatch === current) await openSavedTopic(topicID, note);
  });
  card.append(button);
}

function renderRevert(card, topicID, member, pulls) {
  if (member.merge?.state !== 'merged') return;
  const revert = member.revert;
  if (revert?.state === 'opened') {
    const pull = pulls.find((item) => item.number === revert.pull_number);
    if (pull) {
      const link = element('a', `反向 PR #${revert.pull_number}（到 Gitea 审批）`);
      link.href = pull.url;
      card.append(link);
    } else {
      card.append(element('p', `反向 PR #${revert.pull_number} 已建立；请到仓库核对并审批。`));
    }
    return;
  }
  if (revert?.state === 'manual') {
    card.append(element('p', '无法安全生成反向 PR，请在 Gitea 人工检查。主干未由 Bridge 修改。'));
    return;
  }
  if (!revertEnabled) return;
  const reconcile = revert?.state === 'pull_pending';
  const resumeBranch = revert?.state === 'branch_pending';
  if (revert) card.append(element('p', `反向操作待核对 · ${revert.branch}`));
  const button = element('button', reconcile ? '核对反向 PR' : resumeBranch ? '核对并继续生成反向 PR' : '确认生成反向 PR');
  button.type = 'button';
  button.addEventListener('click', async () => {
    if (mergeBusy || savedBatch?.id !== topicID) return;
    if (!reconcile && !window.confirm(`基于合并提交 ${member.merge.merge_sha} 准备反向 PR？不会合并，也不会直接修改 ${member.target_branch}。`)) return;
    const current = savedBatch;
    mergeBusy = true;
    button.disabled = true;
    message.textContent = '正在处理反向 PR…';
    let note = '已刷新反向 PR 状态';
    try {
      const response = await fetch(`/bridge/api/topics/${encodeURIComponent(topicID)}/members/${member.id}/revert-pr`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ confirm: !reconcile, reconcile, expected_merge: member.merge.merge_sha }),
      });
      if (savedBatch !== current) return;
      if (response.status === 401) return requireLogin();
      if (response.status === 403) note = '当前账号不能生成反向 PR';
      else if (response.status === 409) note = '无法安全自动反向，请在 Gitea 人工检查';
      else if (!response.ok) note = '未收到确认，先核对已保存的状态';
    } catch {
      note = '未收到确认，先核对已保存的状态';
    } finally {
      mergeBusy = false;
      button.disabled = false;
    }
    if (savedBatch === current) await openSavedTopic(topicID, note);
  });
  card.append(button);
}

createPulls.addEventListener('click', async () => {
  const batch = savedBatch;
  if (!batch) return;
  const selected = batch.selected.filter((item) => item.include.checked);
  if (selected.length < 1 || selected.length > 8) {
    message.textContent = '每次请选择 1 到 8 个仓库。';
    return;
  }
  if (!window.confirm(`用你的 Gitea 账号处理选中的 ${selected.length} 个仓库？只建或复用 PR，不合并代码。`)) return;
  createPulls.disabled = true;
  message.textContent = '正在逐仓处理…';
  try {
    const response = await fetch(`/bridge/api/topics/${encodeURIComponent(batch.id)}/pulls`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ confirm: true, member_ids: selected.map((item) => item.member.id) }),
    });
    if (savedBatch !== batch) return;
    if (response.status === 401) return requireLogin();
    if (!response.ok) throw new Error('create pulls failed');
    const updated = await response.json();
    if (savedBatch !== batch) return;
    await openSavedTopic(batch.id, updated.write_denied.length ? `${updated.write_denied.length} 仓不允许你写入` : '已刷新逐仓结果');
  } catch {
    if (savedBatch !== batch) return;
    await openSavedTopic(batch.id, '未收到完整确认，请先看各仓已保存的结果');
  } finally {
    if (savedBatch === batch) createPulls.disabled = false;
  }
});

form.addEventListener('submit', (event) => {
  event.preventDefault();
  lookup();
});

logout.addEventListener('click', async () => {
  try {
    const response = await fetch('/bridge/auth/logout', { method: 'POST' });
    if (!response.ok) throw new Error('logout failed');
    requireLogin();
  } catch {
    message.textContent = '未能退出看板，请稍后重试。';
  }
});

async function loadProjects() {
  try {
    const response = await fetch('/bridge/api/projects');
    if (response.status === 401) return requireLogin();
    if (!response.ok) throw new Error('project list failed');
    const data = await response.json();
    account.textContent = `当前账号：${data.user}`;
    storeEnabled = data.saved_topics;
    writesEnabled = data.pull_creation;
    mergeEnabled = data.merge_enabled;
    revertEnabled = data.revert_enabled;
    logout.hidden = false;
    const params = new URLSearchParams(location.search);
    const saved = storeEnabled && /^[A-Za-z0-9_-]{43}$/.test(params.get('topic_id') ?? '') ? params.get('topic_id') : null;
    if (!data.projects.length) {
      message.textContent = '你暂时没有可查看的项目组。';
      return;
    }
    for (const item of data.projects) {
      const option = element('option', item.name);
      option.value = item.id;
      project.append(option);
    }
    if (saved) {
      await openSavedTopic(saved);
      if (activeQuery?.signal.aborted) return;
    }
    const selected = params.get('project_id');
    if (data.projects.some((item) => item.id === selected)) project.value = selected;
    if (!saved) topic.value = params.get('topic') ?? '';
    form.hidden = false;
    if (!saved) message.textContent = '选择项目组并输入需求编号。';
    if (selected === project.value && topic.value && form.checkValidity()) lookup();
  } catch {
    message.textContent = '暂时无法读取项目，请稍后刷新。';
  }
}

loadProjects();
