/**
 * browse.js — global 3-panel browse (all repos in one left pane).
 *
 * The left pane is server-rendered: each repository appears as a collapsible
 * top-level node. Expanding a node loads its content via the appropriate API,
 * chosen by format:
 *
 *   maven  → hierarchical folder tree   (GET /ui/browse/{repo}/tree?prefix=)
 *   others → flat searchable package list (GET /api/v1/repos/{repo}/components)
 *
 * Center pane: version list  (GET /ui/browse/{repo}/versions?pkg=)
 * Right pane:  asset detail  (GET /ui/browse/{repo}/detail?pkg=&ver=)
 *
 * URL is kept in sync via history.pushState so deep-links work:
 *   /ui/browse           → no repo selected
 *   /ui/browse/{name}    → that repo auto-expanded on load
 *
 * No inline event handlers — all delegation satisfies CSP script-src 'self'.
 */

let currentRepo = '';   // repo context for the currently displayed versions/detail

// ── Repo-level toggle ─────────────────────────────────────────────────────────

function toggleRepo(node) {
  const isOpen = node.classList.contains('expanded');
  // Collapse all open repos first
  document.querySelectorAll('.browse-repo-node.expanded').forEach(n => {
    n.classList.remove('expanded');
    n.querySelector('.browse-tree-toggle').textContent = 'chevron_right';
  });
  if (isOpen) {
    history.pushState(null, '', '/ui/browse');
    return;
  }
  node.classList.add('expanded');
  node.querySelector('.browse-tree-toggle').textContent = 'expand_more';
  history.pushState(null, '', '/ui/browse/' + node.dataset.repo);
  const content = node.querySelector('.browse-repo-content');
  if (!content.dataset.loaded) {
    loadRepoContent(node, content);
  }
}

async function loadRepoContent(repoNode, content) {
  const repo   = repoNode.dataset.repo;
  const format = repoNode.dataset.format;
  const kind   = repoNode.dataset.kind || '';
  content.innerHTML = '<div class="browse-msg">Loading…</div>';
  try {
    if (format === 'maven') {
      await loadTreeLevel(repo, '', 0, content);
    } else {
      await loadFlatPkgs(repo, content, kind);
    }
    content.dataset.loaded = '1';
  } catch (e) {
    content.innerHTML = '<div class="browse-msg browse-err">Failed: ' + esc(String(e)) + '</div>';
  }
}

// ── Flat browse (npm / helm / cran / oci) ────────────────────────────────────

async function loadFlatPkgs(repo, container, kind) {
  const res = await fetch('/api/v1/repos/' + repo + '/components?limit=0');
  if (!res.ok) throw new Error('HTTP ' + res.status);
  const d = await res.json();
  const pkgs = d.components || [];

  // Search input + list wrapper
  container.innerHTML =
    '<div class="browse-search-wrap" style="border-bottom:1px solid var(--border)">' +
    '<input type="search" class="repo-pkg-search" placeholder="Filter…" autocomplete="off">' +
    '</div>' +
    '<div class="repo-pkg-list"></div>' +
    (kind === 'proxy'
      ? '<div class="browse-proxy-lookup">' +
        '<input type="text" class="proxy-pkg-input" placeholder="Look up any package…" autocomplete="off">' +
        '<button class="btn btn-sm proxy-pkg-go">Go</button>' +
        '</div>'
      : '');

  const listEl = container.querySelector('.repo-pkg-list');

  function render(items) {
    listEl.innerHTML = items.length
      ? items.map(p =>
          '<div class="browse-pkg" data-name="' + esc(p.name) + '">' +
          '<span class="ms browse-pkg-icon">package_2</span>' +
          '<span class="browse-pkg-name">' + esc(p.name) + '</span>' +
          '<span class="browse-pkg-ver">' + esc((p.versions && p.versions[0]) || '') + '</span>' +
          '</div>'
        ).join('')
      : '<div class="browse-msg" style="font-size:12px;padding:8px 12px;">' +
        (kind === 'proxy' ? 'No cached packages. Use "Look up" below.' : 'No packages.') +
        '</div>';
  }
  render(pkgs);

  container.querySelector('.repo-pkg-search').addEventListener('input', function() {
    const q = this.value.toLowerCase();
    render(q ? pkgs.filter(p => p.name.toLowerCase().includes(q)) : pkgs);
  });

  // Proxy: direct lookup input
  const goBtn = container.querySelector('.proxy-pkg-go');
  const input = container.querySelector('.proxy-pkg-input');
  if (goBtn && input) {
    const lookup = () => {
      const name = input.value.trim();
      if (name) selectPkg(repo, name);
    };
    goBtn.addEventListener('click', lookup);
    input.addEventListener('keydown', e => { if (e.key === 'Enter') lookup(); });
  }
}

// ── Tree browse (maven) ───────────────────────────────────────────────────────

async function loadTreeLevel(repo, prefix, depth, container) {
  const res = await fetch('/ui/browse/' + repo + '/tree?prefix=' + encodeURIComponent(prefix));
  if (!res.ok) throw new Error('HTTP ' + res.status);
  const nodes = await res.json();
  renderTreeNodes(repo, nodes, depth, container, true);
}

function renderTreeNodes(repo, nodes, depth, container, replace) {
  if (!nodes.length) {
    if (replace) container.innerHTML = '<div class="browse-msg">Empty.</div>';
    return;
  }
  const indent = 12 + depth * 16;
  const html = nodes.map(n => {
    // A node is either a folder to descend (is_dir) or a terminal artifact
    // (component set). Artifacts show a package icon and load versions on click.
    const isComponent = !!n.component;
    const icon = isComponent ? 'package_2' : 'folder';
    return '<div class="browse-tree-node" ' +
      'data-path="' + esc(n.path) + '" ' +
      'data-repo="' + esc(repo) + '" ' +
      'data-is-dir="' + n.is_dir + '" ' +
      (isComponent ? 'data-component="' + esc(n.component) + '" ' : '') +
      'data-depth="' + depth + '" ' +
      'style="padding-left:' + indent + 'px">' +
      (n.is_dir
        ? '<span class="ms browse-tree-toggle">chevron_right</span>'
        : '<span class="browse-tree-toggle-spacer"></span>') +
      '<span class="ms browse-tree-icon">' + icon + '</span>' +
      '<span class="browse-tree-name">' + esc(n.name) + '</span>' +
      '</div>' +
      (n.is_dir
        ? '<div class="browse-tree-children" data-for="' + esc(repo + ':' + n.path) + '" style="display:none"></div>'
        : '');
  }).join('');

  if (replace) container.innerHTML = html;
  else container.insertAdjacentHTML('beforeend', html);
}

async function toggleTreeFolder(repo, node) {
  const path  = node.dataset.path;
  const depth = parseInt(node.dataset.depth, 10);
  const key   = repo + ':' + path;
  const children = document.querySelector('.browse-tree-children[data-for="' + CSS.escape(key) + '"]');
  if (!children) return;

  const toggle = node.querySelector('.browse-tree-toggle');
  const icon   = node.querySelector('.browse-tree-icon');
  const isOpen = children.style.display !== 'none';

  if (isOpen) {
    children.style.display = 'none';
    toggle.textContent = 'chevron_right';
    icon.textContent   = 'folder';
  } else {
    toggle.textContent = 'expand_more';
    icon.textContent   = 'folder_open';
    children.style.display = 'block';
    if (!children.dataset.loaded) {
      children.innerHTML = '<div class="browse-msg" style="padding-left:' + (12 + (depth + 1) * 16) + 'px">Loading…</div>';
      try {
        const res = await fetch('/ui/browse/' + repo + '/tree?prefix=' + encodeURIComponent(path));
        if (!res.ok) throw new Error('HTTP ' + res.status);
        const nodes = await res.json();
        children.innerHTML = '';
        renderTreeNodes(repo, nodes, depth + 1, children, false);
        children.dataset.loaded = '1';
      } catch (e) {
        children.innerHTML = '<div class="browse-msg browse-err">Failed: ' + esc(String(e)) + '</div>';
      }
    }
  }
}

// ── Versions pane ─────────────────────────────────────────────────────────────

async function selectPkg(repo, pkg) {
  currentRepo = repo;
  const cp = document.getElementById('center-pane');
  cp.innerHTML = '<div class="browse-msg">Loading…</div>';
  document.getElementById('detail-pane').innerHTML =
    '<div class="browse-placeholder">' +
    '<span class="ms" style="font-size:40px;color:var(--text-muted)">info</span>' +
    '<p>Select a version.</p></div>';
  try {
    const res = await fetch('/ui/browse/' + repo + '/versions?pkg=' + encodeURIComponent(pkg));
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const d = await res.json();
    renderVersions(d);
  } catch (e) {
    cp.innerHTML = '<div class="browse-msg browse-err">Failed: ' + esc(String(e)) + '</div>';
  }
}

// browseBreadcrumb renders the location trail above the version list:
//   {repo} / {group} / {path} / {artifact}   (maven, component "group.path:artifact")
//   {repo} / {name}                           (flat formats — npm/helm/cran/oci)
// The final segment (the component itself) is emphasised.
function browseBreadcrumb(repo, name) {
  let segs;
  const colon = name.indexOf(':');
  if (colon !== -1) {
    const group = name.slice(0, colon).split('.').filter(Boolean);
    segs = group.concat([name.slice(colon + 1)]);
  } else {
    segs = [name];
  }
  let h = '<div class="browse-breadcrumb"><span class="browse-bc-seg">' + esc(repo) + '</span>';
  segs.forEach((s, i) => {
    h += '<span class="browse-bc-sep">/</span>';
    const cls = i === segs.length - 1 ? 'browse-bc-cur' : 'browse-bc-seg';
    h += '<span class="' + cls + '">' + esc(s) + '</span>';
  });
  return h + '</div>';
}

function renderVersions(d) {
  const cp = document.getElementById('center-pane');
  if (!d.versions || !d.versions.length) {
    cp.innerHTML = '<div class="browse-msg">No versions found.</div>';
    return;
  }
  let h = browseBreadcrumb(currentRepo, d.name);
  h += '<table class="browse-ver-tbl"><thead><tr>' +
       '<th>Version</th><th>Size</th><th>Modified</th>' +
       '</tr></thead><tbody>';
  for (const v of d.versions) {
    const pub = v.published_at && !v.published_at.startsWith('0001') ? v.published_at.substring(0, 10) : '—';
    h += '<tr class="browse-ver-row" data-pkg="' + esc(d.pkg) + '" data-ver="' + esc(v.version) + '">';
    h += '<td class="col-mono">' + esc(v.version) + '</td>';
    h += '<td class="col-mono">' + (v.size_bytes ? fmtBytes(v.size_bytes) : '—') + '</td>';
    h += '<td class="col-date">' + pub + '</td>';
    h += '</tr>';
  }
  h += '</tbody></table>';
  cp.innerHTML = h;
}

// ── Detail pane ───────────────────────────────────────────────────────────────

document.getElementById('center-pane').addEventListener('click', function(e) {
  const row = e.target.closest('.browse-ver-row');
  if (row) selectVer(row.dataset.pkg, row.dataset.ver, row);
});

async function selectVer(pkg, ver, el) {
  document.querySelectorAll('.browse-ver-row').forEach(r => r.classList.remove('active'));
  if (el) el.classList.add('active');
  const dp = document.getElementById('detail-pane');
  dp.innerHTML = '<div class="browse-msg">Loading…</div>';
  try {
    const res = await fetch(
      '/ui/browse/' + currentRepo + '/detail?pkg=' + encodeURIComponent(pkg) + '&ver=' + encodeURIComponent(ver)
    );
    if (!res.ok) throw new Error('HTTP ' + res.status);
    const d = await res.json();
    renderDetail(d);
  } catch (e) {
    dp.innerHTML = '<div class="browse-msg browse-err">Failed: ' + esc(String(e)) + '</div>';
  }
}

function renderDetail(d) {
  const fname = d.file_name || (d.name + '-' + d.version);
  let h = '<div class="browse-detail">';
  h += '<div class="browse-detail-asset-label">Selected asset</div>';
  h += '<div class="browse-detail-filename">' + esc(fname) + '</div>';

  // Actions row — Browse is read-only (consume surface); mutation lives in the
  // repo Content tab. Download + copy only, no delete here.
  h += '<div class="browse-detail-actions">';
  if (d.download_url) {
    h += '<a href="' + esc(d.download_url) + '" class="btn btn-sm btn-primary" style="flex:1;text-align:center;">↓ Download</a>';
    h += '<button class="btn btn-sm btn-icon" id="copy-url-btn" title="Copy URL">⧉</button>';
  }
  h += '</div>';

  // Metadata grid
  const pub = d.published_at && !d.published_at.startsWith('0001') ? d.published_at.substring(0, 10) : null;
  const meta = [
    ['Format',       d.format],
    ['Repository',   d.repo],
    ['Blob store',   d.blob_store],
    ['Size',         d.size_bytes ? fmtBytes(d.size_bytes) : null],
    ['Content-type', d.content_type],
    ['Published',    pub],
  ];
  h += '<dl class="browse-meta">';
  for (const [k, v] of meta) {
    if (v) h += '<dt>' + esc(k) + '</dt><dd>' + esc(v) + '</dd>';
  }
  h += '</dl>';

  // Checksums
  if (d.sha256) {
    h += '<div class="browse-checksum">' +
         '<div class="browse-cksum-label">SHA-256</div>' +
         '<div class="browse-cksum-val">' + esc(d.sha256) + '</div>' +
         '</div>';
  }
  if (d.sha1) {
    h += '<div class="browse-checksum">' +
         '<div class="browse-cksum-label">SHA-1</div>' +
         '<div class="browse-cksum-val">' + esc(d.sha1) + '</div>' +
         '</div>';
  }
  h += '</div>';
  document.getElementById('detail-pane').innerHTML = h;

  const copyBtn = document.getElementById('copy-url-btn');
  if (copyBtn) copyBtn.addEventListener('click', () => navigator.clipboard.writeText(d.download_url));
}

// ── Unified event delegation for the left pane ────────────────────────────────

document.getElementById('pkg-list').addEventListener('click', function(e) {
  // Repo header → toggle expand/collapse
  const hdr = e.target.closest('.browse-repo-hdr');
  if (hdr) {
    toggleRepo(hdr.closest('.browse-repo-node'));
    return;
  }

  // All other clicks need a repo context
  const repoNode = e.target.closest('.browse-repo-node');
  if (!repoNode) return;
  const repo = repoNode.dataset.repo;

  // Flat package item
  const pkg = e.target.closest('.browse-pkg');
  if (pkg) {
    document.querySelectorAll('.browse-pkg').forEach(i => i.classList.remove('active'));
    pkg.classList.add('active');
    selectPkg(repo, pkg.dataset.name);
    return;
  }

  // Maven tree node: folder → expand; artifact (has component) → load versions.
  const tn = e.target.closest('.browse-tree-node');
  if (tn) {
    if (tn.dataset.component) {
      document.querySelectorAll('.browse-tree-node').forEach(n => n.classList.remove('active'));
      tn.classList.add('active');
      selectPkg(repo, tn.dataset.component);
    } else if (tn.dataset.isDir === 'true') {
      toggleTreeFolder(repo, tn);
    }
  }
});

// ── Auto-expand repo from URL ─────────────────────────────────────────────────
// /ui/browse/{repo}           → expand that repo
// /ui/browse/{repo}?pkg={c}   → expand + load that component's versions/detail
// (the deep-link target from the global Search page)
const autoNode = document.querySelector('.browse-repo-node.auto-expand');
if (autoNode) {
  const pkg = new URLSearchParams(window.location.search).get('pkg');
  toggleRepo(autoNode);
  if (pkg) selectPkg(autoNode.dataset.repo, pkg);
}

// ── Shared util ───────────────────────────────────────────────────────────────
function esc(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

function fmtBytes(n) {
  if (n >= 1024 * 1024) return (n / 1024 / 1024).toFixed(2) + ' MB';
  if (n >= 1024)        return (n / 1024).toFixed(1) + ' KB';
  return n + ' B';
}
