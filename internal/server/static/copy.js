document.addEventListener('DOMContentLoaded', function () {
  document.querySelectorAll('[data-copy-id]').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var el = document.getElementById(btn.getAttribute('data-copy-id'));
      if (!el) return;
      navigator.clipboard.writeText(el.textContent).catch(function () {
        try {
          var s = window.getSelection(), r = document.createRange();
          r.selectNode(el);
          s.removeAllRanges();
          s.addRange(r);
          document.execCommand('copy');
        } catch (_) {}
      });
    });
  });
});
