/* Announcement publishing uses the same authenticated admin API as the console. */
let announcementsCache = [], announcementEditID = null, announcementSaving = false;
let announcementInitial = "";

async function loadAnnouncements() {
  const version = requestVersions.announcements = (requestVersions.announcements || 0) + 1;
  try {
    const result = await api("api/announcements");
    if (version !== requestVersions.announcements) return;
    announcementsCache = result;
    renderAnnouncements(); stampRefresh();
  } catch (error) {
    if (version !== requestVersions.announcements) return;
    errorRow("announcementsTable", 4, error);
    reportError(error, "公告加载失败");
  }
}
function renderAnnouncements() {
  const query = $("announcementSearch").value.trim().toLowerCase(), filter = $("announcementFilter").value;
  const list = announcementsCache.filter(item => (!query || (item.title + " " + item.content).toLowerCase().includes(query)) && (!filter || item.published === (filter === "published")));
  $("announcementListCount").textContent = list.length + (list.length !== announcementsCache.length ? " / " + announcementsCache.length : "");
  $("announcementsTable").querySelector("tbody").innerHTML = list.map(item => '<tr><td class="announcement-cell"><strong>' + esc(item.title) + '</strong><p>' + esc(item.content) + '</p></td><td><span class="badge ' + (item.published ? 'on' : 'off') + '">' + (item.published ? '已发布' : '草稿') + '</span>' + (item.pinned ? ' <span class="badge warn">置顶</span>' : '') + '</td><td class="mono">' + (item.published_at ? esc(fmtTime(item.published_at)) : '<span class="muted">尚未发布</span>') + '</td><td><div class="row-actions announcement-actions"><button class="text-btn" onclick="openAnnouncementComposer(' + item.id + ')">' + icon("edit") + '编辑</button><button class="text-btn" onclick="changeAnnouncement(' + item.id + ', {published:' + !item.published + '}, this)">' + (item.published ? '撤下' : '发布') + '</button><button class="text-btn" onclick="changeAnnouncement(' + item.id + ', {pinned:' + !item.pinned + '}, this)">' + (item.pinned ? '取消置顶' : '置顶') + '</button><button class="icon-btn danger" onclick="deleteAnnouncement(' + item.id + ', this)" aria-label="删除 ' + esc(item.title) + '">' + icon("trash") + '</button></div></td></tr>').join("") || emptyRow(4, query || filter ? "没有匹配的公告" : "发布第一条公告", query || filter ? "调整搜索或筛选条件后重试。" : "把最新服务信息分享给主页访客。", query || filter ? "" : "openAnnouncementComposer()", "新建公告", "megaphone");
  labelTableCells("announcementsTable");
}
function announcementFormValue() {
  return JSON.stringify({ title: $("announcementTitle").value, content: $("announcementContent").value, pinned: $("announcementPinned").checked });
}
function announcementDirty() {
  return !$("announcementComposer").classList.contains("hidden") && announcementFormValue() !== announcementInitial;
}
function updateAnnouncementPreview() {
  $("announcementPreviewTitle").textContent = $("announcementTitle").value.trim() || "公告标题";
  $("announcementPreviewContent").textContent = $("announcementContent").value.trim() || "公告正文将在这里显示。";
}
async function openAnnouncementComposer(id = null) {
  if (announcementSaving) return;
  if (announcementDirty() && !await askAction({ title: "放弃未保存的公告？", description: "当前表单的修改尚未保存。", confirm: "放弃修改", danger: true })) return;
  const item = id === null ? null : announcementsCache.find(item => item.id === id);
  if (id !== null && !item) { toast("公告不存在，请刷新后重试", "error"); return; }
  announcementEditID = id;
  $("announcementForm").reset();
  $("announcementForm").querySelectorAll(".form-error").forEach(error => error.classList.add("hidden"));
  ["announcementTitle", "announcementContent"].forEach(id => { $(id).setCustomValidity(""); $(id).removeAttribute("aria-invalid"); });
  $("announcementTitle").value = item?.title || "";
  $("announcementContent").value = item?.content || "";
  $("announcementPinned").checked = !!item?.pinned;
  $("announcementComposerTitle").textContent = item ? "编辑公告" : "新建公告";
  $("announcementDraftButton").textContent = item?.published ? "保存并撤下" : "保存草稿";
  $("announcementPublishButton").innerHTML = icon("megaphone") + (item?.published ? " 保存并发布" : " 发布公告");
  $("announcementComposer").querySelector("details").open = false;
  announcementInitial = announcementFormValue();
  updateAnnouncementPreview();
  $("announcementComposer").classList.remove("hidden");
  $("announcementTitle").focus();
}
function resetAnnouncementComposer() {
  $("announcementComposer").classList.add("hidden");
  $("announcementForm").reset();
  announcementEditID = null; announcementInitial = "";
}
async function closeAnnouncementComposer() {
  if (announcementSaving) return;
  if (announcementDirty() && !await askAction({ title: "放弃未保存的公告？", description: "可继续编辑并保存为草稿，稍后再发布。", confirm: "放弃修改", danger: true })) return;
  resetAnnouncementComposer();
}
async function saveAnnouncement(event) {
  event.preventDefault();
  if (announcementSaving) return false;
  for (const id of ["announcementTitle", "announcementContent"]) {
    const field = $(id);
    field.setCustomValidity(field.value.trim() ? "" : (id === "announcementTitle" ? "请填写公告标题" : "请填写公告正文"));
  }
  if (!$("announcementForm").reportValidity()) return false;
  const published = event.submitter?.dataset.publish === "true";
  const id = announcementEditID, button = event.submitter || $("announcementDraftButton");
  const wasPublished = announcementsCache.find(item => item.id === id)?.published;
  const body = { title: $("announcementTitle").value.trim(), content: $("announcementContent").value.trim(), pinned: $("announcementPinned").checked, published };
  announcementSaving = true;
  $("announcementFields").disabled = true;
  $("announcementDraftButton").disabled = true;
  $("announcementPublishButton").disabled = true;
  $("announcementFormError").classList.add("hidden");
  setBusy(button, true, published ? "正在发布…" : "正在保存…");
  try {
    await api(id === null ? "api/announcements" : "api/announcements/" + id, { method: id === null ? "POST" : "PATCH", body: JSON.stringify(body) });
    resetAnnouncementComposer();
    toast(published ? "公告已发布，主页将在下次更新时显示" : wasPublished ? "公告已撤下并保存为草稿" : "草稿已保存");
    await loadAnnouncements();
  } catch (error) {
    if (error.message !== "unauthorized") {
      $("announcementFormError").textContent = error.message;
      $("announcementFormError").classList.remove("hidden");
      $("announcementFormError").focus();
    }
  } finally {
    announcementSaving = false;
    $("announcementFields").disabled = false;
    setBusy(button, false);
    $("announcementDraftButton").disabled = false;
    $("announcementPublishButton").disabled = false;
  }
  return false;
}
async function changeAnnouncement(id, changes, button) {
  setBusy(button, true, "处理中…");
  try {
    const item = await api("api/announcements/" + id, { method: "PATCH", body: JSON.stringify(changes) });
    toast(changes.published !== undefined ? (item.published ? "公告已发布" : "公告已撤下，保留为草稿") : (item.pinned ? "公告已置顶" : "已取消置顶"));
    await loadAnnouncements();
  } catch (error) { reportError(error); }
  finally { setBusy(button, false); }
}
async function deleteAnnouncement(id, button) {
  const item = announcementsCache.find(item => item.id === id);
  if (!await askAction({ title: "删除公告？", description: "“" + (item?.title || "此公告") + "”将被永久删除。也可以通过撤下保留为草稿。", confirm: "删除公告", danger: true })) return;
  setBusy(button, true, "");
  try {
    await api("api/announcements/" + id, { method: "DELETE" });
    if (announcementEditID === id) resetAnnouncementComposer();
    await loadAnnouncements(); toast("公告已删除");
  } catch (error) { reportError(error); }
  finally { setBusy(button, false); }
}
function initAnnouncements() {
  $("announcementForm").addEventListener("input", event => { event.target.setCustomValidity?.(""); updateAnnouncementPreview(); });
}
