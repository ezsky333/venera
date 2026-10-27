package api

// adminHTML 是管理面板的单文件实现。
//
// 为什么内嵌而不是单独的前端工程：
// 这个服务跑在 NAS 的 Docker 里，镜像大小与部署复杂度都是真实约束。
// 一个不依赖构建链、零外部资源的页面，比一个 vite 工程更合适。
// 页面只用原生 JS + fetch，所有数据来自 /api/v1/admin/*。
const adminHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Venera NAS 管理面板</title>
<style>
  :root{
    --bg:#0f1115; --panel:#171a21; --panel2:#1e222b; --line:#2a2f3a;
    --fg:#e6e8ee; --dim:#8b93a7; --accent:#4f8cff; --ok:#3fb950;
    --warn:#d29922; --err:#f85149;
  }
  *{box-sizing:border-box}
  body{margin:0;font:14px/1.5 -apple-system,BlinkMacSystemFont,"Segoe UI","Noto Sans CJK SC",sans-serif;
       background:var(--bg);color:var(--fg)}
  a{color:var(--accent);text-decoration:none}
  header{position:sticky;top:0;z-index:10;display:flex;gap:12px;align-items:center;
         padding:12px 20px;background:rgba(15,17,21,.92);backdrop-filter:blur(8px);
         border-bottom:1px solid var(--line)}
  header h1{font-size:16px;margin:0;font-weight:600;white-space:nowrap}
  header input{flex:1;max-width:320px;padding:7px 10px;border-radius:7px;
               border:1px solid var(--line);background:var(--panel2);color:var(--fg)}
  header button{padding:7px 14px;border-radius:7px;border:1px solid var(--line);
                background:var(--panel2);color:var(--fg);cursor:pointer}
  header button:hover{border-color:var(--accent)}
  nav{display:flex;gap:4px;padding:0 20px;border-bottom:1px solid var(--line);
      background:var(--panel);overflow-x:auto}
  nav button{padding:11px 16px;border:0;background:none;color:var(--dim);cursor:pointer;
             border-bottom:2px solid transparent;white-space:nowrap;font-size:14px}
  nav button.on{color:var(--fg);border-bottom-color:var(--accent)}
  main{padding:20px;max-width:1400px;margin:0 auto}
  .stats{display:grid;grid-template-columns:repeat(auto-fit,minmax(150px,1fr));gap:12px;margin-bottom:20px}
  .stat{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:14px 16px}
  .stat .k{color:var(--dim);font-size:12px;margin-bottom:6px}
  .stat .v{font-size:22px;font-weight:600;font-variant-numeric:tabular-nums}
  .stat .v small{font-size:12px;color:var(--dim);font-weight:400;margin-left:4px}
  table{width:100%;border-collapse:collapse;background:var(--panel);
        border:1px solid var(--line);border-radius:10px;overflow:hidden}
  th,td{padding:10px 12px;text-align:left;border-bottom:1px solid var(--line);vertical-align:middle}
  th{color:var(--dim);font-weight:500;font-size:12px;text-transform:uppercase;letter-spacing:.03em}
  tr:last-child td{border-bottom:0}
  tbody tr:hover{background:var(--panel2)}
  .cover{width:44px;height:60px;object-fit:cover;border-radius:5px;background:var(--panel2);display:block}
  .btn{padding:5px 11px;border-radius:6px;border:1px solid var(--line);
       background:var(--panel2);color:var(--fg);cursor:pointer;font-size:12px}
  .btn:hover{border-color:var(--accent)}
  .btn.danger:hover{border-color:var(--err);color:var(--err)}
  .badge{display:inline-block;padding:1px 8px;border-radius:999px;font-size:11px;
         border:1px solid var(--line);color:var(--dim)}
  .badge.ok{color:var(--ok);border-color:rgba(63,185,80,.4)}
  .badge.err{color:var(--err);border-color:rgba(248,81,73,.4)}
  .badge.warn{color:var(--warn);border-color:rgba(210,153,34,.4)}
  .badge.run{color:var(--accent);border-color:rgba(79,140,255,.4)}
  .empty{padding:40px;text-align:center;color:var(--dim)}
  .muted{color:var(--dim);font-size:12px}
  .bar{height:6px;background:var(--panel2);border-radius:3px;overflow:hidden;margin-top:6px}
  .bar>i{display:block;height:100%;background:var(--accent);transition:width .3s}
  .row{display:flex;gap:10px;align-items:center;flex-wrap:wrap;margin-bottom:14px}
  .row select,.row input{padding:7px 10px;border-radius:7px;border:1px solid var(--line);
                         background:var(--panel2);color:var(--fg)}
  dialog{background:var(--panel);color:var(--fg);border:1px solid var(--line);
         border-radius:12px;padding:0;max-width:1000px;width:94vw}
  dialog::backdrop{background:rgba(0,0,0,.6)}
  .dlg-h{display:flex;justify-content:space-between;align-items:center;
         padding:14px 18px;border-bottom:1px solid var(--line)}
  .dlg-b{padding:18px;max-height:65vh;overflow:auto}
  .grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(110px,1fr));gap:8px}
  .grid img{width:100%;border-radius:6px;background:var(--panel2)}
</style>
</head>
<body>

<header>
  <h1>Venera NAS</h1>
  <input id="token" type="password" placeholder="访问令牌（Bearer token）" autocomplete="off">
  <button id="save">连接</button>
  <span id="status" class="muted"></span>
</header>

<nav>
  <button data-tab="overview" class="on">总览</button>
  <button data-tab="library">漫画库</button>
  <button data-tab="jobs">下载任务</button>
  <button data-tab="folders">收藏夹</button>
</nav>

<main>
  <section id="tab-overview">
    <div class="empty">填写访问令牌后点「连接」加载数据</div>
  </section>
  <section id="tab-library" hidden></section>
  <section id="tab-jobs" hidden></section>
  <section id="tab-folders" hidden></section>
</main>

<dialog id="dlg">
  <div class="dlg-h"><strong id="dlg-title">详情</strong>
    <button class="btn" onclick="document.getElementById('dlg').close()">关闭</button></div>
  <div class="dlg-b" id="dlg-body"></div>
</dialog>

<script>
const $ = (s, r) => (r || document).querySelector(s);
const el = (tag, props, ...kids) => {
  const n = document.createElement(tag);
  if (props) for (const k in props) {
    if (k === 'class') n.className = props[k];
    else if (k === 'html') n.innerHTML = props[k];
    else if (k.startsWith('on')) n.addEventListener(k.slice(2), props[k]);
    else if (props[k] != null) n.setAttribute(k, props[k]);
  }
  for (const c of kids.flat()) if (c != null) n.append(c.nodeType ? c : document.createTextNode(c));
  return n;
};
const fmtBytes = (b) => {
  if (!b) return '0 B';
  const u = ['B','KB','MB','GB','TB'];
  let i = 0, v = b;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 100 || i === 0 ? 0 : 1) + ' ' + u[i];
};
const fmtTime = (s) => s ? new Date(s).toLocaleString('zh-CN') : '—';
const esc = (s) => String(s == null ? '' : s).replace(/[&<>"']/g, c => (
  {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));

let TOKEN = sessionStorage.getItem('nas_token') || '';
// 支持 /admin?token=xxx 直接进入（便利贴链接场景；token 仍只在浏览器本地保存）
{
  const q = new URLSearchParams(location.search);
  if (q.get('token')) {
    TOKEN = q.get('token');
    // 立刻从地址栏抹掉 token，避免被复制/截图泄漏。
    // 注意必须带上 hash：只写 pathname 会把 #library 这类路由一起清掉。
    history.replaceState(null, '', location.pathname + location.hash);
  }
}
$('#token').value = TOKEN;

async function api(path, opts) {
  const r = await fetch(path, Object.assign({
    headers: {
      'Authorization': 'Bearer ' + TOKEN,
      'Content-Type': 'application/json'
    }
  }, opts || {}));
  if (!r.ok) {
    let msg = 'HTTP ' + r.status;
    try { const j = await r.json(); if (j.error) msg = j.error; } catch (_) {}
    throw new Error(msg);
  }
  return r.json();
}

function setStatus(text, kind) {
  const s = $('#status');
  s.textContent = text || '';
  s.style.color = kind === 'err' ? 'var(--err)' : kind === 'ok' ? 'var(--ok)' : 'var(--dim)';
}

$('#save').onclick = () => {
  TOKEN = $('#token').value.trim();
  sessionStorage.setItem('nas_token', TOKEN);
  loadAll();
};

// ---------- 总览 ----------
async function loadOverview() {
  const root = $('#tab-overview');
  const st = await api('/api/v1/admin/stats');

  const cards = [
    ['库大小', fmtBytes(st.libraryBytes)],
    ['库内漫画', st.libraryComics, '本'],
    ['库内章节', st.libraryChapters, '章'],
    ['库内页数', st.libraryPages, '页'],
    ['收藏夹', st.favoriteFolders, '个'],
    ['收藏漫画', st.favoriteItems, '项'],
    ['待下载', st.jobsPending, '个'],
    ['下载中', st.jobsRunning, '个'],
    ['已完成', st.jobsDone, '个'],
    ['失败', st.jobsFailed, '个'],
  ];

  const box = el('div', {class:'stats'});
  for (const [k, v, unit] of cards) {
    box.append(el('div', {class:'stat'},
      el('div', {class:'k'}, k),
      el('div', {class:'v'}, String(v), unit ? el('small', null, unit) : null)
    ));
  }

  // 下载进度（占队列比例）
  const total = st.jobsPending + st.jobsRunning + st.jobsDone + st.jobsFailed;
  const pct = total ? Math.round(st.jobsDone * 100 / total) : 0;
  const prog = el('div', {class:'stat'}, 
    el('div', {class:'k'}, '整体进度'),
    el('div', {class:'v'}, pct + '%'),
    el('div', {class:'bar'}, el('i', {style:'width:' + pct + '%'}))
  );

  root.replaceChildren(
    box,
    el('div', {class:'stats'}, prog),
    el('div', {class:'muted'}, '同步游标：' + st.cursor + ' · 已同步设备：' + st.clientCount + ' 台')
  );
}

// ---------- 漫画库 ----------
let libOffset = 0, libSource = '';
async function loadLibrary(reset) {
  if (reset) libOffset = 0;
  const root = $('#tab-library');
  const data = await api('/api/v1/admin/library?limit=40&offset=' + libOffset +
                         (libSource ? '&source=' + encodeURIComponent(libSource) : ''));

  const rows = (data.items || []).map(it => el('tr', null,
    el('td', null, it.cover ? el('img', {class:'cover', src:it.cover, loading:'lazy',
      onerror: function(){ this.style.visibility='hidden'; }}) : el('div', {class:'cover'})),
    el('td', null,
      el('div', {style:'font-weight:500'}, it.title || it.comicId),
      el('div', {class:'muted'}, it.source + ' · ' + it.comicId)),
    el('td', null, it.chapters + ' 章'),
    el('td', null, it.pages + ' 页'),
    el('td', null, fmtBytes(it.bytes)),
    el('td', {class:'muted'}, fmtTime(it.updatedAt)),
    el('td', null,
      el('button', {class:'btn', onclick: () => showComic(it)}, '查看'),
      ' ',
      el('button', {class:'btn danger', onclick: () => deleteComic(it)}, '删除')
    )
  ));

  const table = el('table', null,
    el('thead', null, el('tr', null,
      el('th', null, '封面'), el('th', null, '标题'), el('th', null, '章节'),
      el('th', null, '页数'), el('th', null, '大小'), el('th', null, '更新时间'), el('th', null, '操作'))),
    el('tbody', null, rows.length ? rows : el('tr', null, el('td', {colspan:7},
      el('div', {class:'empty'}, libSource
        ? '该来源下没有已缓存的漫画。' + (libOffset > 0 ? '' : '换一个来源，或等下载任务完成后刷新。')
        : '库里还没有漫画。等下载任务完成后刷新。'))))
  );

  const pager = el('div', {class:'row', style:'margin-top:14px'},
    el('button', {class:'btn', disabled: libOffset === 0 ? '' : null,
      onclick: () => { libOffset = Math.max(0, libOffset - 40); loadLibrary(); }}, '上一页'),
    el('span', {class:'muted'}, (libOffset + 1) + '–' + Math.min(libOffset + 40, data.total) + ' / ' + data.total),
    el('button', {class:'btn', disabled: libOffset + 40 >= data.total ? '' : null,
      onclick: () => { libOffset += 40; loadLibrary(); }}, '下一页')
  );

  const srcSel = el('select', {onchange: (e) => { libSource = e.target.value; loadLibrary(true); }},
    el('option', {value: ''}, '全部来源'));
  const st = await api('/api/v1/sources');
  for (const s of (st.sources || [])) {
    const o = el('option', {value: s.key}, s.name + ' (' + s.key + ')');
    if (s.key === libSource) o.selected = true;
    srcSel.append(o);
  }

  root.replaceChildren(el('div', {class:'row'}, srcSel), table, pager);
}

// 点击章节 → 内嵌阅读视图：拉 /api/v1/chapter 的页 URL 列表，逐页展示。
async function previewChapter(it, ch) {
  const body = $('#dlg-body');
  body.replaceChildren(
    el('div', {class:'row'},
      el('button', {class:'btn', onclick: () => showComic(it)}, '← 返回'),
      el('strong', null, ch.title || ch.chapterId),
      el('span', {class:'muted'}, (ch.pages || 0) + ' 页'))
  );
  const wrap = el('div', {style:'margin-top:12px'});
  body.append(wrap);
  try {
    const d = await api('/api/v1/chapter?source=' + encodeURIComponent(it.source) +
      '&comicId=' + encodeURIComponent(it.comicId) +
      '&chapterId=' + encodeURIComponent(ch.chapterId));
    if (!d.cached || !(d.pages || []).length) {
      wrap.append(el('div', {class:'empty'}, '这一章在 NAS 上没有缓存页面。'));
      return;
    }
    for (const p of d.pages) {
      const img = el('img', {src: p, loading: 'lazy',
        style: 'display:block;width:100%;max-width:900px;margin:0 auto 8px;border-radius:6px;background:var(--panel2)',
        onerror: function(){ this.style.background='var(--err)'; this.alt='加载失败'; }});
      wrap.append(img);
    }
  } catch (e) {
    wrap.append(el('div', {class:'empty'}, '加载失败：' + e.message));
  }
}

async function showComic(it) {
  const d = await api('/api/v1/admin/comic?source=' + encodeURIComponent(it.source) +
                      '&comicId=' + encodeURIComponent(it.comicId));
  $('#dlg-title').textContent = d.title || it.comicId;
  const body = $('#dlg-body');
  body.replaceChildren();

  if (d.cover) body.append(el('img', {src:d.cover, style:'width:140px;border-radius:8px;float:left;margin-right:16px'}));
  body.append(el('div', {style:'margin-bottom:8px'}, el('strong', null, d.title || it.comicId)));
  if (d.subtitle) body.append(el('div', {class:'muted'}, d.subtitle));
  if (d.tags && d.tags.length) body.append(el('div', {class:'muted', style:'margin-top:6px'}, d.tags.join(' · ')));
  body.append(el('div', {style:'clear:both;height:14px'}));

  // 缓存概况：章节/页数/体积来自库列表行，弹窗里再给一条摘要
  body.append(el('div', {class:'muted', style:'margin-bottom:8px'},
    '缓存概况：' + it.chapters + ' 章 · ' + it.pages + ' 页 · ' + fmtBytes(it.bytes) +
    ' · 更新于 ' + fmtTime(it.updatedAt)));

  const chs = d.chapters || [];
  body.append(el('div', {class:'muted', style:'margin-bottom:8px'},
    '已缓存 ' + chs.length + ' 章（点章节名可在线预览）'));
  const t = el('table', null,
    el('thead', null, el('tr', null,
      el('th', null, '章节'), el('th', null, '页数'), el('th', null, '大小'),
      el('th', null, '时间'), el('th', null, '操作'))),
    el('tbody', null, chs.map(c => el('tr', null,
      el('td', null,
        (c.pages === 0
          ? el('span', {class:'badge warn'}, '空 ')
          : el('a', {href:'#', onclick: (e) => { e.preventDefault(); previewChapter(it, c); }},
             c.title || c.chapterId))),
      el('td', null, String(c.pages)),
      el('td', null, fmtBytes(c.bytes)),
      el('td', {class:'muted'}, fmtTime(c.createdAt)),
      el('td', null,
        c.pages > 0 ? el('button', {class:'btn', onclick: () => previewChapter(it, c)}, '阅读') : null,
        ' ',
        el('button', {class:'btn danger', onclick: async () => {
          if (!confirm('删除章节《' + (c.title || c.chapterId) + '》的缓存？')) return;
          await api('/api/v1/admin/chapter/delete', {method:'POST', body: JSON.stringify({
            source: it.source, comicId: it.comicId, chapterId: c.chapterId
          })});
          showComic(it);
          loadLibrary();
        }}, '删除'))
    )))
  );
  body.append(t);
  $('#dlg').showModal();
}

async function deleteComic(it) {
  if (!confirm('确定删除《' + (it.title || it.comicId) + '》的全部缓存？\n这会同时删除磁盘文件。')) return;
  await api('/api/v1/admin/comic/delete', {method:'POST', body: JSON.stringify({
    source: it.source, comicId: it.comicId
  })});
  loadLibrary();
}

// ---------- 任务 ----------
let jobOffset = 0, jobStatus = '', jobTotal = 0;
const JOB_PAGE = 50;

async function loadJobs(reset) {
  if (reset) jobOffset = 0;
  const root = $('#tab-jobs');
  const data = await api('/api/v1/admin/jobs?limit=' + JOB_PAGE + '&offset=' + jobOffset +
                         (jobStatus ? '&status=' + encodeURIComponent(jobStatus) : ''));
  const list = data.jobs || [];
  jobTotal = data.total || list.length;

  const cls = (s) => s === 'done' ? 'ok' : s === 'failed' ? 'err'
                   : s === 'running' ? 'run'
                   : s === 'paused' || s === 'cancelled' ? 'warn' : '';
  const label = (s) => ({done:'完成', failed:'失败', running:'下载中',
                          pending:'等待中', paused:'已暂停', cancelled:'已取消'}[s] || s);

  const rows = list.map(j => {
    const canPause = j.status === 'pending' || j.status === 'running';
    const canResume = j.status === 'paused';
    const canCancel = j.status === 'pending' || j.status === 'paused';
    const canRetry = j.status === 'failed' || j.status === 'cancelled';
    // 运行中的任务不能删：worker 靠 jobs 行里的 message 判断该不该停，
    // 行被删掉后它就再也停不下来（会继续把整本下完）。先暂停再删。
    const canDelete = j.status !== 'running';
    const p = Math.round((j.progress || 0) * 100);
    // data-job-id 让 applyLive() 能原地找到这一行做实时刷新：
    // 只改进度条/百分比/速度/页数/信息，不重建 DOM（aria2 式顺滑的关键）
    return el('tr', {'data-job-id': String(j.id)},
      el('td', {class:'muted'},
        (j.priority ? '📌 ' : '') + '#' + j.id),
      el('td', null, j.source),
      el('td', null,
        el('div', {}, j.title || j.comicId),
        j.error ? el('div', {class:'muted', style:'color:var(--err)'},
          String(j.error).slice(0, 120)) : null),
      el('td', null, el('span', {class:'badge ' + cls(j.status)}, label(j.status))),
      el('td', null,
        el('div', {class:'muted'},
          el('span', {class:'js-pct'}, p + '%'),
          el('span', {class:'js-speed'}),
          el('span', {class:'js-pages', style:'margin-left:6px'})),
        el('div', {class:'bar'}, el('i', {style:'width:' + p + '%'}))),
      el('td', {class:'muted js-msg'}, j.message || '—'),
      el('td', null,
        canPause ? el('button', {class:'btn', onclick: () => jobAction(j.id, 'pause')}, '暂停') : null,
        canResume ? el('button', {class:'btn', onclick: () => jobAction(j.id, 'resume')}, '继续') : null,
        j.status === 'pending' ? el('button', {class:'btn',
          onclick: () => jobAction(j.id, j.priority ? 'untop' : 'top')},
          j.priority ? '取消置顶' : '置顶') : null,
        canCancel ? el('button', {class:'btn danger', onclick: () => jobAction(j.id, 'cancel')}, '取消') : null,
        canRetry ? el('button', {class:'btn', onclick: () => jobAction(j.id, 'retry')}, '重试') : null,
        canDelete ? el('button', {class:'btn danger', onclick: () => {
          if (!confirm('删除任务 #' + j.id + '？\n\n只删除这条任务记录，'
                       + '不会删除已下载的缓存文件。')) return;
          jobAction(j.id, 'delete');
        }}, '删除') : null)
    );
  });

  const statusSel = el('select', {onchange: (e) => { jobStatus = e.target.value; loadJobs(true); }},
    ...[['', '全部状态'], ['pending', '等待中'], ['running', '下载中'], ['paused', '已暂停'],
        ['done', '完成'], ['failed', '失败'], ['cancelled', '已取消']].map(([v, t]) => {
      const o = el('option', {value: v}, t);
      if (v === jobStatus) o.selected = true;
      return o;
    }));

  const pager = el('div', {class:'row', style:'margin-top:14px'},
    el('button', {class:'btn', disabled: jobOffset === 0 ? '' : null,
      onclick: () => { jobOffset = Math.max(0, jobOffset - JOB_PAGE); loadJobs(); }}, '上一页'),
    el('span', {class:'muted'},
      (jobOffset + 1) + '–' + Math.min(jobOffset + JOB_PAGE, jobTotal) + ' / ' + jobTotal),
    el('button', {class:'btn', disabled: jobOffset + JOB_PAGE >= jobTotal ? '' : null,
      onclick: () => { jobOffset += JOB_PAGE; loadJobs(); }}, '下一页'),
    el('span', {class:'muted', style:'margin-left:auto', id:'sse-state'}, '')
  );

  root.replaceChildren(
    el('div', {class:'row'},
      statusSel,
      el('button', {class:'btn', onclick: async () => {
        if (!confirm('把所有等待中的任务暂停？')) return;
        await api('/api/v1/jobs/bulk', {method:'POST', body:'{"action":"pause-all"}'});
        loadJobs(true);
      }}, '全部暂停'),
      el('button', {class:'btn', onclick: async () => {
        await api('/api/v1/jobs/bulk', {method:'POST', body:'{"action":"resume-all"}'});
        loadJobs(true);
      }}, '全部继续'),
      el('button', {class:'btn', onclick: async () => {
        const r = await api('/api/v1/jobs/bulk', {method:'POST', body:'{"action":"retry-failed"}'});
        setStatus('重新排队 ' + r.affected + ' 个失败任务', 'ok');
        loadJobs(true);
      }}, '重试失败任务'),
      el('button', {class:'btn', onclick: backfillFavorites}, '补录未缓存收藏'),
      el('button', {class:'btn danger', onclick: async () => {
        // 只清终态（完成/失败/已取消）。等待中和已暂停的是用户还要下载的
        // 东西，运行中的不能删（见行内删除按钮的说明）。
        if (!confirm('清理所有「完成 / 失败 / 已取消」的任务记录？\n\n'
                     + '不会删除已下载的缓存文件；正在下载的任务会保留。')) return;
        const r = await api('/api/v1/jobs/bulk', {method:'POST',
          body:'{"action":"delete-finished"}'});
        setStatus('已清理 ' + r.affected + ' 条任务记录', 'ok');
        loadJobs(true);
      }}, '清理已完成任务'),
      el('button', {class:'btn', onclick: () => loadJobs(true)}, '刷新')
    ),
    el('table', null,
      el('thead', null, el('tr', null,
        el('th', null, 'ID'), el('th', null, '源'), el('th', null, '漫画'),
        el('th', null, '状态'), el('th', null, '进度'), el('th', null, '信息'), el('th', null, '操作'))),
      el('tbody', null, rows.length ? rows : el('tr', null,
        el('td', {colspan:7}, el('div', {class:'empty'},
          '该状态下暂无任务。'))))
    ),
    pager
  );
  startJobSse();
}

async function jobAction(id, action) {
  try {
    await api('/api/v1/jobs/' + id + '/' + action, {method:'POST', body:'{}'});
    loadJobs();
  } catch (e) {
    setStatus('操作失败：' + e.message, 'err');
  }
}

// 补录"收藏了但没缓存"的漫画。
//
// 两步：先 dryRun 拿计划（会入队多少本、跳过了多少、哪些书），把明细
// 摊给用户看，确认后再真正入队。不能直接一键入队——收藏可能几百本，
// 而真正没下过的也许只有个位数，盲点一下就往队列里塞几百条。
async function backfillFavorites() {
  let plan;
  try {
    plan = await api('/api/v1/jobs/queue-favorites?dryRun=1');
  } catch (e) {
    setStatus('预览失败：' + e.message, 'err');
    return;
  }
  const bySource = Object.entries(plan.bySource || {})
    .map(([k, v]) => k + ':' + v).join('  ');
  if (!plan.candidates) {
    setStatus('没有需要补录的漫画（收藏里都已缓存或已在队列中）', 'ok');
    return;
  }
  const preview = (plan.items || []).slice(0, 15)
    .map((it, i) => (i + 1) + '. [' + it.source + '] ' + (it.title || it.comicId))
    .join('\n');
  const more = plan.candidates > 15
    ? '\n… 还有 ' + (plan.candidates - 15) + ' 本' : '';
  const msg = '将补录 ' + plan.candidates + ' 本漫画到下载队列\n\n'
    + '按源: ' + bySource + '\n'
    + '已跳过: ' + plan.alreadyCached + ' 本已缓存, '
    + plan.alreadyQueued + ' 本已在队列\n\n'
    + '前几本:\n' + preview + more + '\n\n确定开始下载？';
  if (!confirm(msg)) return;
  try {
    const r = await api('/api/v1/jobs/queue-favorites', {method:'POST', body:'{}'});
    setStatus('已入队 ' + r.queued + ' 本'
      + (r.failed ? '，失败 ' + r.failed + ' 本' : ''), 'ok');
    loadJobs(true);
  } catch (e) {
    setStatus('补录失败：' + e.message, 'err');
  }
}

// SSE 实时更新：任务页可见时订阅 /api/v1/jobs/events。
//
// 两类事件：
//   live —— 实时进度帧（服务端 200ms 合并推送），直接原地更新进度条，
//           **不重查接口**，所以能像 aria2 一样顺滑；
//   jobs —— 任务列表快照（仅状态跃变时推），这时才重渲染表格。
let jobSse = null;
let liveMap = {};          // jobId → 最新实时状态
let lastJobsRefresh = 0;

function fmtSpeed(bps) {
  if (!bps || bps <= 0) return '';
  const u = ['B/s','KB/s','MB/s'];
  let v = bps, i = 0;
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++; }
  return v.toFixed(v >= 100 || i === 0 ? 0 : 1) + ' ' + u[i];
}

// 用 liveMap 原地刷新表格里已存在的行（不做 DOM 重建）。
function applyLive() {
  const tbody = $('#tab-jobs tbody');
  if (!tbody) return;
  for (const tr of tbody.children) {
    const id = Number(tr.dataset.jobId || 0);
    if (!id) continue;
    const l = liveMap[id];
    if (!l || l.status !== 'running') continue;
    const pct = Math.max(0, Math.min(99, Math.round((l.progress || 0) * 100)));
    const bar = tr.querySelector('.bar > i');
    if (bar) bar.style.width = pct + '%';
    const pctEl = tr.querySelector('.js-pct');
    if (pctEl) pctEl.textContent = pct + '%';
    const msgEl = tr.querySelector('.js-msg');
    if (msgEl && l.message) msgEl.textContent = l.message;
    const spEl = tr.querySelector('.js-speed');
    if (spEl) {
      const sp = fmtSpeed(l.speedBps);
      spEl.textContent = sp ? ' · ' + sp : '';
    }
    const pgEl = tr.querySelector('.js-pages');
    if (pgEl && l.imageTotal > 0) {
      pgEl.textContent = l.imageDone + '/' + l.imageTotal + ' 页';
    }
  }
}

function startJobSse() {
  if (!$('#tab-jobs') || $('#tab-jobs').hidden) return;
  if (jobSse) return; // 已在订阅
  try { jobSse = new EventSource('/api/v1/jobs/events?token=' + encodeURIComponent(TOKEN)); } catch (_) { return; }
  const stEl = () => $('#sse-state');

  jobSse.addEventListener('live', (ev) => {
    try {
      const frame = JSON.parse(ev.data) || [];
      liveMap = {};
      let running = 0;
      for (const l of frame) {
        liveMap[l.jobId] = l;
        if (l.status === 'running') running++;
      }
      applyLive();
      const e = stEl();
      if (e) {
        let totalSpeed = 0;
        for (const l of frame) if (l.status === 'running') totalSpeed += (l.speedBps || 0);
        const sp = fmtSpeed(totalSpeed);
        e.textContent = '实时 · 运行 ' + running + (sp ? ' · ' + sp : '');
      }
    } catch (_) {}
  });

  jobSse.addEventListener('jobs', () => {
    // 列表快照：状态集合变了才重渲染（限速，避免与 live 帧互相干扰）
    const now = Date.now();
    if (now - lastJobsRefresh < 400) return;
    lastJobsRefresh = now;
    if (!$('#tab-jobs').hidden) loadJobs().catch(()=>{});
  });

  jobSse.onerror = () => {
    if (jobSse) { jobSse.close(); jobSse = null; }
    const e = stEl(); if (e) e.textContent = '实时连接断开';
    setTimeout(() => { if (!$('#tab-jobs').hidden) startJobSse(); }, 5000);
  };
}
function stopJobSse() {
  if (jobSse) { jobSse.close(); jobSse = null; }
  liveMap = {};
  const e = $('#sse-state'); if (e) e.textContent = '';
}

// ---------- 收藏夹 ----------
async function loadFolders() {
  const root = $('#tab-folders');
  const data = await api('/api/v1/admin/folders');
  const list = data.folders || [];
  root.replaceChildren(
    list.length ? el('table', null,
      el('thead', null, el('tr', null,
        el('th', null, '收藏夹'), el('th', null, '收藏数'), el('th', null, '已缓存'))),
      el('tbody', null, list.map(f => el('tr', null,
        el('td', null, f.folder),
        el('td', null, String(f.count)),
        el('td', null,
          el('span', {class:'badge ' + (f.cached > 0 ? 'ok' : '')},
            f.cached + ' / ' + f.count))
      )))
    ) : el('div', {class:'empty'}, '暂无收藏夹数据（先在 App 里连接 NAS 完成一次同步）')
  );
}

// ---------- 路由 ----------
const loaders = {overview: loadOverview, library: () => loadLibrary(true),
                 jobs: loadJobs, folders: loadFolders};

async function showTab(name) {
  if (!loaders[name]) name = 'overview';
  document.querySelectorAll('nav button').forEach(x =>
    x.classList.toggle('on', x.dataset.tab === name));
  for (const tab of ['overview','library','jobs','folders']) {
    $('#tab-' + tab).hidden = (tab !== name);
  }
  if (name !== 'jobs') stopJobSse();
  if (location.hash.slice(1) !== name) history.replaceState(null, '', '#' + name);
  try { await loaders[name](); setStatus(''); }
  catch (e) { setStatus(e.message, 'err'); }
}

document.querySelectorAll('nav button').forEach(b => {
  b.onclick = () => showTab(b.dataset.tab);
});
window.addEventListener('hashchange', () => showTab(location.hash.slice(1) || 'overview'));

async function loadAll() {
  if (!TOKEN) { setStatus('请填写访问令牌', 'err'); return; }
  try {
    const p = await api('/api/v1/info');
    setStatus('已连接 · ' + p.sourceCount + ' 个源 · 库 ' + fmtBytes(p.libraryBytes), 'ok');
    await showTab(location.hash.slice(1) || 'overview');
  } catch (e) {
    setStatus('连接失败：' + e.message, 'err');
  }
}

if (TOKEN) loadAll(); else setStatus('填写访问令牌后点「连接」', null);

// 任务页由 SSE 实时驱动（startJobSse）；这里保留一个低频兜底轮询，
// SSE 不可用的浏览器（极少）也能刷新。
setInterval(() => {
  if (!$('#tab-jobs').hidden && TOKEN && !jobSse) loadJobs().catch(()=>{});
}, 5000);
</script>
</body>
</html>
`
