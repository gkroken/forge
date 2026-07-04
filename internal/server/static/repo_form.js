// Repository create/manage form. Shows only the field groups relevant to the
// selected kind (hosted / proxy / group): each group is tagged
// data-kind-show="<kind> [<kind> …]" and hidden unless the current kind
// matches. Loaded as an external script because the page CSP forbids inline
// handlers (script-src 'self'); the old inline syncKind() never ran.
(function () {
  'use strict';

  function initKindScoping() {
    var kindSel = document.getElementById('f-kind');
    if (!kindSel) return; // not on a repo form

    var groups = Array.prototype.slice.call(document.querySelectorAll('[data-kind-show]'));

    function apply() {
      var kind = kindSel.value;
      groups.forEach(function (el) {
        var kinds = el.getAttribute('data-kind-show').split(/[\s,]+/);
        el.style.display = kinds.indexOf(kind) !== -1 ? '' : 'none';
      });
    }

    kindSel.addEventListener('change', apply);
    apply();
  }

  // Group member picker. The server renders every eligible candidate (non-group,
  // not self) as a hidden-until-matched checkbox; show only those whose format
  // matches the group's own, and uncheck the rest so a hidden box never submits.
  // The format can change live on the new-repo form, so re-filter on its change.
  function initMembersPicker() {
    var picker = document.getElementById('members-picker');
    if (!picker) return;

    var opts = Array.prototype.slice.call(picker.querySelectorAll('.member-opt'));
    var emptyHint = document.querySelector('#g-members .member-empty');
    var fmtEl = document.getElementById('f-format'); // <select> (new) or readonly <input> (edit)

    function currentFormat() { return fmtEl ? fmtEl.value : ''; }

    function apply() {
      var fmt = currentFormat();
      var shown = 0;
      opts.forEach(function (o) {
        var match = o.getAttribute('data-format') === fmt;
        o.style.display = match ? '' : 'none';
        if (!match) {
          var cb = o.querySelector('input[type=checkbox]');
          if (cb) cb.checked = false;
        } else {
          shown++;
        }
      });
      if (emptyHint) emptyHint.style.display = shown ? 'none' : '';
    }

    if (fmtEl && fmtEl.tagName === 'SELECT') fmtEl.addEventListener('change', apply);
    apply();
  }

  document.addEventListener('DOMContentLoaded', function () {
    initKindScoping();
    initMembersPicker();
  });
})();
