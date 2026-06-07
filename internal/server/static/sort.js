// Sortable tables: add data-sortable to <table>, data-sort to <th> headers.
// A <span class="sort-arrow"></span> inside the <th> shows the current direction.
// Re-initializes after htmx swaps so filtered/live-search tables keep working.

(function () {
  function init() {
    document.querySelectorAll('table[data-sortable] thead th[data-sort]').forEach(function (th) {
      if (th._sortBound) return;
      th._sortBound = true;
      th.classList.add('col-sortable');
      th.addEventListener('click', function () { sortBy(th); });
    });
  }

  function sortBy(activeTh) {
    var table = activeTh.closest('table');
    var headers = Array.from(table.querySelectorAll('thead th'));
    var colIdx = headers.indexOf(activeTh);
    var asc = activeTh.getAttribute('aria-sort') !== 'ascending';

    headers.forEach(function (th) {
      th.removeAttribute('aria-sort');
      var arrow = th.querySelector('.sort-arrow');
      if (arrow) arrow.textContent = '';
    });

    activeTh.setAttribute('aria-sort', asc ? 'ascending' : 'descending');
    var arrow = activeTh.querySelector('.sort-arrow');
    if (arrow) arrow.textContent = asc ? ' ▲' : ' ▼';

    var tbody = table.querySelector('tbody');
    var rows = Array.from(tbody.querySelectorAll('tr'));

    rows.sort(function (a, b) {
      var av = (a.cells[colIdx] ? a.cells[colIdx].textContent : '').trim();
      var bv = (b.cells[colIdx] ? b.cells[colIdx].textContent : '').trim();
      return av.localeCompare(bv, undefined, {numeric: true, sensitivity: 'base'}) * (asc ? 1 : -1);
    });

    rows.forEach(function (r) { tbody.appendChild(r); });
  }

  document.addEventListener('DOMContentLoaded', init);
  document.addEventListener('htmx:afterSwap', init);
})();
