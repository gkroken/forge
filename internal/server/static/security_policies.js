(function () {
  var root = document.getElementById('secpol-root');
  if (!root || !document.getElementById('secpol-tbody')) return;

  var API = '/api/v1/security-policies';
  var SEVERITIES = ['low', 'moderate', 'high', 'critical'];

  function esc(s) {
    return String(s == null ? '' : s)
      .replace(/&/g, '&amp;').replace(/</g, '&lt;')
      .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
  }

  // ── Toast ──────────────────────────────────────────────────────────────────
  var toastWrap;
  function toast(msg, type) {
    if (!toastWrap) {
      toastWrap = document.createElement('div');
      toastWrap.className = 'toast-container';
      document.body.appendChild(toastWrap);
    }
    var t = document.createElement('div');
    t.className = 'toast toast-' + (type || 'ok');
    t.textContent = msg;
    toastWrap.appendChild(t);
    setTimeout(function () { t.remove(); }, 4000);
  }

  function modePill(mode) {
    var m = (mode || 'off').toLowerCase();
    var cls = m === 'block' ? 'chip-err' : m === 'warn' ? 'chip-warn' : 'chip-neutral';
    var label = m === 'block' ? 'Block' : m === 'warn' ? 'Warn' : 'Off';
    return '<span class="chip ' + cls + '">' + label + '</span>';
  }

  // ── Global default ─────────────────────────────────────────────────────────
  function loadDefault() {
    fetch(API + '/_default').then(function (r) { return r.json(); }).then(function (p) {
      document.getElementById('def-mode').value = (p.mode || 'off');
      document.getElementById('def-threshold').value = (p.threshold || 'high');
      document.getElementById('def-failopen').checked = !!p.failOpen;
    }).catch(function () {});
  }

  function initDefaultForm() {
    var form = document.getElementById('secpol-default-form');
    if (!form) return;
    form.addEventListener('submit', function (e) {
      e.preventDefault();
      var body = {
        mode: document.getElementById('def-mode').value,
        threshold: document.getElementById('def-threshold').value,
        failOpen: document.getElementById('def-failopen').checked
      };
      fetch(API + '/_default', {
        method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body)
      }).then(function (r) {
        if (!r.ok) throw new Error(r.status);
        var saved = document.getElementById('def-saved');
        if (saved) { saved.textContent = 'Saved'; setTimeout(function () { saved.textContent = ''; }, 2500); }
        toast('Global default saved', 'ok');
      }).catch(function () { toast('Could not save the default policy', 'err'); });
    });
  }

  // ── Named policies table ───────────────────────────────────────────────────
  function loadPolicies() {
    var tbody = document.getElementById('secpol-tbody');
    fetch(API).then(function (r) { return r.json(); }).then(function (list) {
      if (!list || !list.length) {
        tbody.innerHTML = '<tr><td colspan="6" style="text-align:center;padding:24px;color:var(--text-muted);">' +
          'No named policies yet. Create one to apply the same rule across repositories.</td></tr>';
        return;
      }
      tbody.innerHTML = list.map(function (p) {
        var supp = (p.suppressions || []).length;
        return '<tr>' +
          '<td><strong>' + esc(p.name) + '</strong>' +
            (p.description ? '<div style="font-size:11px;color:var(--text-muted);">' + esc(p.description) + '</div>' : '') + '</td>' +
          '<td>' + modePill(p.mode) + '</td>' +
          '<td style="text-transform:capitalize;">' + esc(p.threshold || 'high') + '</td>' +
          '<td>' + (p.failOpen ? 'Serve' : '<span style="color:var(--danger);">Block</span>') + '</td>' +
          '<td>' + (supp ? esc(supp) : '—') + '</td>' +
          '<td style="text-align:right;">' +
            '<button class="btn btn-sm" data-edit="' + esc(p.name) + '">Edit</button> ' +
            '<button class="btn btn-sm btn-danger" data-del="' + esc(p.name) + '">Delete</button>' +
          '</td></tr>';
      }).join('');
      bindRowButtons(list);
    }).catch(function () {
      tbody.innerHTML = '<tr><td colspan="6" style="text-align:center;padding:24px;color:var(--text-muted);">Failed to load policies.</td></tr>';
    });
  }

  function bindRowButtons(list) {
    var byName = {};
    list.forEach(function (p) { byName[p.name] = p; });
    document.querySelectorAll('[data-edit]').forEach(function (b) {
      b.addEventListener('click', function () { openEditor(byName[b.getAttribute('data-edit')]); });
    });
    document.querySelectorAll('[data-del]').forEach(function (b) {
      b.addEventListener('click', function () {
        var name = b.getAttribute('data-del');
        if (!window.confirm('Delete the "' + name + '" policy? Repositories using it fall back to the global default.')) return;
        fetch(API + '/' + encodeURIComponent(name), { method: 'DELETE' }).then(function (r) {
          if (!r.ok && r.status !== 204) throw new Error(r.status);
          toast('Deleted "' + name + '"', 'ok');
          loadPolicies();
        }).catch(function () { toast('Could not delete the policy', 'err'); });
      });
    });
  }

  // ── Editor modal ───────────────────────────────────────────────────────────
  var modal;
  function openEditor(existing) {
    var p = existing || { name: '', description: '', mode: 'block', threshold: 'high', failOpen: true, suppressions: [] };
    var isNew = !existing;
    if (!modal) {
      modal = document.createElement('div');
      modal.className = 'modal-overlay hidden';
      document.body.appendChild(modal);
    }
    modal.innerHTML =
      '<div class="modal-box" style="max-width:520px;">' +
        '<div class="modal-title">' + (isNew ? 'New security policy' : 'Edit "' + esc(p.name) + '"') + '</div>' +
        '<div class="modal-body">' +
          '<div class="form-group"><label>Name</label>' +
            '<input id="ed-name" type="text" value="' + esc(p.name) + '" ' + (isNew ? '' : 'readonly') +
            ' placeholder="e.g. strict"></div>' +
          '<div class="form-group"><label>Description</label>' +
            '<input id="ed-desc" type="text" value="' + esc(p.description) + '" placeholder="optional"></div>' +
          '<div class="form-row">' +
            '<div class="form-group"><label>Enforcement</label><select id="ed-mode">' +
              opt('off', 'Off', p.mode) + opt('warn', 'Warn', p.mode) + opt('block', 'Block', p.mode) +
            '</select></div>' +
            '<div class="form-group"><label>Act at severity</label><select id="ed-threshold">' +
              SEVERITIES.map(function (s) { return opt(s, cap(s), p.threshold); }).join('') +
            '</select></div>' +
          '</div>' +
          '<div class="form-group"><label class="checkbox-label">' +
            '<input id="ed-failopen" type="checkbox" ' + (p.failOpen ? 'checked' : '') + '> ' +
            'Serve unscanned artifacts (fail open)</label></div>' +
          '<div class="form-group"><label>Suppressed advisories</label>' +
            '<p class="form-hint" style="margin-top:0;">One CVE/GHSA id per line, optionally <code>id — reason</code>. Suppressed advisories never count toward severity.</p>' +
            '<textarea id="ed-supp" rows="3" style="width:100%;font-family:\'IBM Plex Mono\',monospace;font-size:12px;">' +
              esc((p.suppressions || []).map(function (s) { return s.reason ? s.id + ' — ' + s.reason : s.id; }).join('\n')) +
            '</textarea></div>' +
        '</div>' +
        '<div class="modal-footer">' +
          '<button class="btn" id="ed-cancel">Cancel</button>' +
          '<button class="btn btn-primary" id="ed-save">' + (isNew ? 'Create policy' : 'Save changes') + '</button>' +
        '</div>' +
      '</div>';
    modal.classList.remove('hidden');
    modal.querySelector('#ed-cancel').onclick = close;
    modal.onclick = function (e) { if (e.target === modal) close(); };
    modal.querySelector('#ed-save').onclick = function () { save(isNew, p.name); };
  }
  function close() { if (modal) modal.classList.add('hidden'); }
  function opt(v, label, cur) {
    return '<option value="' + v + '"' + (v === (cur || '') ? ' selected' : '') + '>' + label + '</option>';
  }
  function cap(s) { return s.charAt(0).toUpperCase() + s.slice(1); }

  function parseSuppressions(text) {
    return text.split('\n').map(function (line) {
      var t = line.trim();
      if (!t) return null;
      var parts = t.split(/\s+[—-]\s+/); // "id — reason" or "id - reason"
      return { id: parts[0].trim(), reason: parts.length > 1 ? parts.slice(1).join(' - ').trim() : '' };
    }).filter(Boolean);
  }

  function save(isNew, origName) {
    var name = (isNew ? document.getElementById('ed-name').value : origName).trim();
    if (!name) { toast('A policy needs a name', 'err'); return; }
    var body = {
      name: name,
      description: document.getElementById('ed-desc').value.trim(),
      mode: document.getElementById('ed-mode').value,
      threshold: document.getElementById('ed-threshold').value,
      failOpen: document.getElementById('ed-failopen').checked,
      suppressions: parseSuppressions(document.getElementById('ed-supp').value)
    };
    var url = isNew ? API : API + '/' + encodeURIComponent(name);
    fetch(url, {
      method: isNew ? 'POST' : 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body)
    }).then(function (r) {
      if (!r.ok) return r.text().then(function (t) { throw new Error(t || r.status); });
      toast('Saved "' + name + '"', 'ok');
      close();
      loadPolicies();
    }).catch(function (e) { toast('Could not save: ' + e.message, 'err'); });
  }

  document.addEventListener('DOMContentLoaded', function () {
    var newBtn = document.getElementById('secpol-new');
    if (newBtn) newBtn.addEventListener('click', function () { openEditor(null); });
    initDefaultForm();
    loadDefault();
    loadPolicies();
  });
})();
