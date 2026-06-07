document.addEventListener('DOMContentLoaded', function () {
  document.querySelectorAll('.version-toggle').forEach(function (btn) {
    var target = document.getElementById(btn.getAttribute('data-target'));
    if (!target) return;
    var collapsedHeight = getComputedStyle(target).maxHeight;
    var expanded = false;
    btn.addEventListener('click', function () {
      expanded = !expanded;
      if (expanded) {
        target.style.maxHeight = target.scrollHeight + 'px';
        target.classList.add('is-expanded');
        btn.textContent = 'Show fewer ▴';
      } else {
        target.style.maxHeight = collapsedHeight;
        target.classList.remove('is-expanded');
        btn.textContent = btn.getAttribute('data-label');
      }
    });
  });
});
