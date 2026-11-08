// Copy buttons and confirmations. The admin UI works without this file.
document.addEventListener("click", (e) => {
  const b = e.target.closest("[data-copy]");
  if (!b) return;
  const el = document.getElementById(b.dataset.copy);
  const done = () => { b.textContent = "Copied"; setTimeout(() => { b.textContent = "Copy"; }, 1500); };
  if (navigator.clipboard && window.isSecureContext) {
    navigator.clipboard.writeText(el.textContent.trim()).then(done);
  } else {
    // Plain-http pages cannot use the clipboard API; select the text instead.
    const r = document.createRange();
    r.selectNodeContents(el);
    const s = getSelection();
    s.removeAllRanges();
    s.addRange(r);
    b.textContent = "Press Ctrl+C";
  }
});
document.addEventListener("submit", (e) => {
  const m = e.target.dataset.confirm;
  if (m && !confirm(m)) e.preventDefault();
});
