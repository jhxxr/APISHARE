/* Shared clipboard support for HTTPS and ordinary HTTP deployments. */
async function copyToClipboard(value) {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(value);
      return;
    } catch {
      // A denied Clipboard API request can still allow copying selected text.
    }
  }

  const field = document.createElement("textarea"), active = document.activeElement;
  field.value = value;
  field.readOnly = true;
  field.tabIndex = -1;
  field.setAttribute("aria-hidden", "true");
  field.style.cssText = "position:fixed;top:0;left:0;width:1px;height:1px;padding:0;border:0;font-size:16px;opacity:0;pointer-events:none";
  (document.querySelector("dialog[open]") || document.body).append(field);
  try {
    field.focus({ preventScroll: true });
    field.select();
    field.setSelectionRange(0, field.value.length);
    if (!document.execCommand("copy")) throw new Error("浏览器未允许复制，请检查权限后重试");
  } finally {
    field.remove();
    if (active?.isConnected) active.focus({ preventScroll: true });
  }
}
