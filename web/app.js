const form = document.querySelector('#lookup');
const login = document.querySelector('#login');
const logout = document.querySelector('#logout');
const account = document.querySelector('#account-name');
const message = document.querySelector('#message');
const results = document.querySelector('#results');
const submit = form.querySelector('button');
const project = form.elements.namedItem('project_id');
const topic = form.elements.namedItem('topic');
const statuses = { complete: '已查完', incomplete: '未查完', failed: '查询失败' };
const lifecycles = { merged: '已合并', open: '开放', closed: '关闭未合并' };
let activeQuery;

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
  results.replaceChildren();
  message.textContent = '先登录 Gitea，再查看你有权限的仓库。';
}

function showResults(data) {
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
      line.append(element('p', 'CI：未知 · 冲突：未知 · 审批：未知 · 变更文件数：未知'));
      card.append(line);
    }
    results.append(card);
  }
}

async function lookup() {
  activeQuery?.abort();
  const query = new AbortController();
  activeQuery = query;
  const params = new URLSearchParams(new FormData(form));
  history.replaceState(null, '', `/bridge/?${params}`);
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
    if (!query.signal.aborted) showResults(data);
  } catch {
    if (!query.signal.aborted) message.textContent = '暂时查不到结果，请稍后重试。';
  } finally {
    if (activeQuery === query) submit.disabled = false;
  }
}

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
    logout.hidden = false;
    if (!data.projects.length) {
      message.textContent = '你暂时没有可查看的项目组。';
      return;
    }
    for (const item of data.projects) {
      const option = element('option', item.name);
      option.value = item.id;
      project.append(option);
    }
    const params = new URLSearchParams(location.search);
    const selected = params.get('project_id');
    if (data.projects.some((item) => item.id === selected)) project.value = selected;
    topic.value = params.get('topic') ?? '';
    form.hidden = false;
    message.textContent = '选择项目组并输入需求编号。';
    if (selected === project.value && topic.value && form.checkValidity()) lookup();
  } catch {
    message.textContent = '暂时无法读取项目，请稍后刷新。';
  }
}

loadProjects();
