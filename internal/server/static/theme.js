// Apply saved theme preference before first paint to avoid flash.
(function () {
  var t = localStorage.getItem('forge-theme');
  if (t) document.documentElement.setAttribute('data-theme', t);
})();

// Wire the toggle button once the DOM is ready.
document.addEventListener('DOMContentLoaded', function () {
  var btn = document.getElementById('theme-toggle');
  if (!btn) return;

  function isDark() {
    var attr = document.documentElement.getAttribute('data-theme');
    if (attr === 'dark') return true;
    if (attr === 'light') return false;
    return window.matchMedia('(prefers-color-scheme: dark)').matches;
  }

  function setLabel() {
    btn.textContent = isDark() ? 'Light mode' : 'Dark mode';
  }

  btn.addEventListener('click', function () {
    var next = isDark() ? 'light' : 'dark';
    var sysDark = window.matchMedia('(prefers-color-scheme: dark)').matches;
    // If toggling back to the OS default, clear the override entirely.
    if ((next === 'dark') === sysDark) {
      document.documentElement.removeAttribute('data-theme');
      localStorage.removeItem('forge-theme');
    } else {
      document.documentElement.setAttribute('data-theme', next);
      localStorage.setItem('forge-theme', next);
    }
    setLabel();
  });

  setLabel();
});
