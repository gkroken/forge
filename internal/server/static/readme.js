(function () {
  var el = document.getElementById('pkg-readme');
  if (!el || typeof marked === 'undefined' || typeof DOMPurify === 'undefined') return;
  el.innerHTML = DOMPurify.sanitize(marked.parse(el.textContent));
  el.classList.add('readme-rendered');
}());
