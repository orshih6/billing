// Confirm destructive actions: <form data-confirm="Void this invoice?">.
document.addEventListener("submit", function (e) {
  var msg = e.target.getAttribute("data-confirm");
  if (msg && !window.confirm(msg)) e.preventDefault();
});

// Copy buttons: <button data-copy="#id">.
document.addEventListener("click", function (e) {
  var btn = e.target.closest("[data-copy]");
  if (!btn) return;
  var el = document.querySelector(btn.getAttribute("data-copy"));
  if (!el || !navigator.clipboard) return;
  navigator.clipboard.writeText(el.textContent.trim()).then(function () {
    var old = btn.textContent;
    btn.textContent = "Copied";
    setTimeout(function () { btn.textContent = old; }, 1500);
  });
});

// Auto-submit selects: <select data-autosubmit>.
document.addEventListener("change", function (e) {
  if (e.target.hasAttribute("data-autosubmit")) e.target.form.submit();
});
