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

  document.addEventListener('DOMContentLoaded', initKindScoping);
})();
