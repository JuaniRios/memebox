const $ = (sel) => document.querySelector(sel);

const state = {
  items: [],
  presets: [],
  captions: false,
  kind: "",
  tag: "",
  query: "",
  current: null, // item open in the dialog
  result: null, // { file, url, preset }
};

// ---------- helpers ----------

function toast(msg, ms = 2600) {
  const el = $("#toast");
  el.textContent = msg;
  el.classList.add("show");
  clearTimeout(toast.timer);
  toast.timer = setTimeout(() => el.classList.remove("show"), ms);
}

async function api(path, opts = {}) {
  const res = await fetch(path, opts);
  if (res.status === 401) {
    location.href = "/login";
    throw new Error("log in first");
  }
  if (!res.ok) {
    let msg = `HTTP ${res.status}`;
    try { msg = (await res.json()).error || msg; } catch {}
    throw new Error(msg);
  }
  return res;
}

function fmtBytes(n) {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(0)} KB`;
  return `${(n / 1024 / 1024).toFixed(1)} MB`;
}

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") node.className = v;
    else if (k === "muted") node.muted = true;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else node.setAttribute(k, v);
  }
  node.append(...children);
  return node;
}

const kindLabel = { still: "", animated: "GIF", video: "VIDEO" };

// ---------- library ----------

async function loadItems() {
  state.items = await (await api("/api/items")).json();
  render();
}

function matches(it) {
  if (state.kind && it.kind !== state.kind) return false;
  if (state.tag && !it.tags.includes(state.tag)) return false;
  const words = state.query.toLowerCase().split(/\s+/).filter(Boolean);
  const hay = `${it.name} ${it.tags.join(" ")}`.toLowerCase();
  return words.every((w) => hay.includes(w));
}

function render() {
  const grid = $("#grid");
  const shown = state.items.filter(matches);
  grid.replaceChildren(
    ...shown.map((it) =>
      el("button", { class: "card", type: "button", title: it.name, onclick: () => openItem(it.id) },
        el("img", { src: `/media/preview/${it.id}`, alt: it.name, loading: "lazy", decoding: "async" }),
        kindLabel[it.kind] ? el("span", { class: "badge" }, kindLabel[it.kind] + (it.hasAudio ? " 🔊" : "")) : "",
        el("span", { class: "card-name" }, it.name),
      ),
    ),
  );
  $("#empty").hidden = state.items.length > 0;
  if (state.items.length > 0 && shown.length === 0) {
    grid.append(el("p", { class: "muted none" }, "Nothing matches."));
  }
  renderTags();
}

function renderTags() {
  const counts = new Map();
  for (const it of state.items) for (const t of it.tags) counts.set(t, (counts.get(t) || 0) + 1);
  const top = [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 24).map(([t]) => t);
  if (state.tag && !top.includes(state.tag)) top.unshift(state.tag);
  $("#tag-chips").replaceChildren(
    ...top.map((t) =>
      el("button", {
        type: "button",
        class: "chip tag" + (state.tag === t ? " active" : ""),
        onclick: () => { state.tag = state.tag === t ? "" : t; render(); },
      }, "#" + t),
    ),
  );
}

$("#search").addEventListener("input", (e) => { state.query = e.target.value; render(); });
$("#filters").addEventListener("click", (e) => {
  const b = e.target.closest("[data-kind]");
  if (!b) return;
  state.kind = b.dataset.kind;
  document.querySelectorAll("[data-kind]").forEach((x) => x.classList.toggle("active", x === b));
  render();
});

// ---------- adding ----------

async function uploadFiles(files) {
  files = [...files].filter((f) => f.size > 0);
  if (!files.length) return;
  const form = new FormData();
  form.append("tags", $("#add-tags").value);
  for (const f of files) form.append("file", f, f.name || "pasted.png");
  toast(`Adding ${files.length} file${files.length > 1 ? "s" : ""}…`, 60000);
  try {
    const data = await (await api("/api/items", { method: "POST", body: form })).json();
    const n = data.items.length;
    toast(`Added ${n} meme${n > 1 ? "s" : ""}` + (data.failures?.length ? ` · failed: ${data.failures.join("; ")}` : ""), 4000);
    $("#add-dialog").close();
    await loadItems();
    if (n === 1) openItem(data.items[0].id);
  } catch (err) {
    toast(`Upload failed: ${err.message}`, 6000);
  }
}

$("#add-btn").addEventListener("click", () => $("#add-dialog").showModal());
$("#pick-files").addEventListener("click", () => $("#file-input").click());
$("#file-input").addEventListener("change", (e) => { uploadFiles(e.target.files); e.target.value = ""; });

$("#url-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const url = $("#url-input").value.trim();
  toast("Importing…", 60000);
  try {
    const data = await (await api("/api/import", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ url, tags: $("#add-tags").value.split(/[,\s]+/).filter(Boolean) }),
    })).json();
    $("#url-input").value = "";
    $("#add-dialog").close();
    toast("Imported");
    await loadItems();
    openItem(data.items[0].id);
  } catch (err) {
    toast(`Import failed: ${err.message}`, 6000);
  }
});

document.addEventListener("paste", (e) => {
  const files = [...(e.clipboardData?.files || [])];
  if (files.length) {
    e.preventDefault();
    uploadFiles(files);
    return;
  }
  const text = e.clipboardData?.getData("text") || "";
  const inField = e.target.closest?.("input, textarea");
  if (!inField && /^https?:\/\//.test(text.trim())) {
    $("#add-dialog").showModal();
    $("#url-input").value = text.trim();
  }
});

let dragDepth = 0;
const hasFiles = (e) => [...(e.dataTransfer?.types || [])].includes("Files");
document.addEventListener("dragenter", (e) => { if (hasFiles(e)) { dragDepth++; $("#drop-overlay").hidden = false; } });
document.addEventListener("dragleave", (e) => { if (hasFiles(e) && --dragDepth <= 0) { dragDepth = 0; $("#drop-overlay").hidden = true; } });
document.addEventListener("dragover", (e) => { if (hasFiles(e)) e.preventDefault(); });
document.addEventListener("drop", (e) => {
  if (!hasFiles(e)) return;
  e.preventDefault();
  dragDepth = 0;
  $("#drop-overlay").hidden = true;
  uploadFiles(e.dataTransfer.files);
});

// ---------- item dialog ----------

function openItem(id) {
  const it = state.items.find((x) => x.id === id);
  if (!it) return;
  state.current = it;
  clearResult();
  $("#item-name").value = it.name;
  $("#item-tags").value = it.tags.join(", ");
  const src = `/media/original/${it.id}`;
  const media = it.kind === "video"
    ? el("video", { src, controls: "", loop: "", playsinline: "", autoplay: "", muted: "" })
    : el("img", { src, alt: it.name });
  $("#item-preview").replaceChildren(media);
  const bits = [it.kind === "still" ? "image" : it.kind, `${it.width}×${it.height}`, fmtBytes(it.size)];
  if (it.duration) bits.push(`${it.duration.toFixed(1)} s`);
  if (it.hasAudio) bits.push("sound");
  if (it.uses) bits.push(`sent ${it.uses}×`);
  $("#item-meta").textContent = bits.join(" · ");
  $("#item-download").href = `${src}?download=1`;

  $("#preset-buttons").replaceChildren(
    ...state.presets.filter((p) => p.kinds.includes(it.kind)).map((p) =>
      el("button", { type: "button", class: "preset", "data-preset": p.id, onclick: () => exportItem(p) }, p.label),
    ),
  );
  $("#trim-fields").hidden = it.kind === "still";
  $("#caption-fields").hidden = !state.captions;
  $("#item-dialog").showModal();
  // showModal focuses the name field, which pops the keyboard on phones.
  document.activeElement?.blur();
}

async function saveMeta() {
  const it = state.current;
  if (!it) return;
  const name = $("#item-name").value.trim();
  const tags = $("#item-tags").value.split(/[,]+|\s+/).filter(Boolean);
  if (name === it.name && tags.join(",") === it.tags.join(",")) return;
  try {
    const updated = await (await api(`/api/items/${it.id}`, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name, tags }),
    })).json();
    Object.assign(it, updated);
    $("#item-tags").value = updated.tags.join(", ");
    render();
  } catch (err) {
    toast(`Save failed: ${err.message}`);
  }
}
$("#item-name").addEventListener("change", saveMeta);
$("#item-tags").addEventListener("change", saveMeta);
$("#item-name").addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); e.target.blur(); } });

$("#item-delete").addEventListener("click", async () => {
  const it = state.current;
  if (!it || !confirm(`Delete “${it.name}”?`)) return;
  try {
    await api(`/api/items/${it.id}`, { method: "DELETE" });
    $("#item-dialog").close();
    toast("Deleted");
    await loadItems();
  } catch (err) {
    toast(`Delete failed: ${err.message}`);
  }
});

$("#item-dialog").addEventListener("close", () => {
  saveMeta();
  clearResult();
  $("#item-preview").replaceChildren();
  state.current = null;
});

// ---------- export ----------

function exportURL(presetID, extra = {}) {
  const q = new URLSearchParams();
  const top = $("#opt-top").value.trim();
  const bottom = $("#opt-bottom").value.trim();
  if (state.captions && top) q.set("top", top);
  if (state.captions && bottom) q.set("bottom", bottom);
  q.set("fit", $("#opt-fit").value);
  if (state.current.kind !== "still") {
    if (+$("#opt-start").value > 0) q.set("start", $("#opt-start").value);
    if (+$("#opt-dur").value > 0) q.set("dur", $("#opt-dur").value);
  }
  for (const [k, v] of Object.entries(extra)) q.set(k, v);
  return `/api/items/${state.current.id}/export/${presetID}?${q}`;
}

function clearResult() {
  if (state.result?.url) URL.revokeObjectURL(state.result.url);
  state.result = null;
  $("#result").hidden = true;
  $("#result-preview").replaceChildren();
  document.querySelectorAll(".preset.active").forEach((b) => b.classList.remove("active"));
}

async function exportItem(preset) {
  const it = state.current;
  clearResult();
  document.querySelector(`.preset[data-preset="${preset.id}"]`)?.classList.add("active");
  $("#working").hidden = false;
  try {
    const url = exportURL(preset.id);
    const res = await api(url);
    const blob = await res.blob();
    if (state.current !== it) return;
    const filename = res.headers.get("X-Memebox-Filename") || `meme-${preset.id}`;
    const limit = +res.headers.get("X-Memebox-Limit") || 0;
    const file = new File([blob], filename, { type: blob.type });
    const objURL = URL.createObjectURL(file);
    state.result = { file, url: objURL, preset, path: url };

    const isVideo = blob.type.startsWith("video/");
    $("#result-preview").replaceChildren(isVideo
      ? el("video", { src: objURL, autoplay: "", loop: "", muted: "", playsinline: "", controls: "" })
      : el("img", { src: objURL, alt: filename }));
    let info = `${filename} · ${fmtBytes(file.size)}`;
    if (limit && file.size > limit) info += ` · over the ${fmtBytes(limit)} limit, try trimming it`;
    $("#result-info").textContent = info;
    $("#result-info").classList.toggle("warn", !!(limit && file.size > limit));
    $("#result-hint").textContent = preset.hint;
    $("#download-btn").href = objURL;
    $("#download-btn").download = filename;
    $("#share-btn").hidden = !(navigator.canShare && navigator.canShare({ files: [file] }));
    $("#result").hidden = false;
    $("#result").scrollIntoView({ block: "nearest", behavior: "smooth" });
  } catch (err) {
    toast(`Conversion failed: ${err.message}`, 7000);
  } finally {
    $("#working").hidden = true;
  }
}

$("#share-btn").addEventListener("click", async () => {
  const file = state.result?.file;
  if (!file) return;
  try {
    await navigator.share({ files: [file] });
  } catch (err) {
    if (err.name !== "AbortError") toast(`Share failed: ${err.message}. Use Download instead.`, 5000);
  }
});

// Browsers only put PNG on the clipboard, so moving formats copy a still
// frame. The PNG is fetched inside the click so Safari keeps the gesture.
$("#copy-btn").addEventListener("click", async () => {
  const r = state.result;
  if (!r) return;
  if (!navigator.clipboard?.write || typeof ClipboardItem === "undefined") {
    toast("This browser cannot copy images. Use Share or Download.");
    return;
  }
  const isPNG = r.file.type === "image/png";
  const png = isPNG
    ? Promise.resolve(r.file)
    : api(exportURL("png", { track: "0" })).then((res) => res.blob());
  try {
    await navigator.clipboard.write([new ClipboardItem({ "image/png": png })]);
    toast(isPNG ? "Copied, paste it into any chat" : "Copied a still frame. Use Share or Download to keep the animation.", 4000);
  } catch (err) {
    toast(`Copy failed: ${err.message}`);
  }
});

$("#save-btn").addEventListener("click", async () => {
  const r = state.result;
  if (!r) return;
  try {
    const created = await (await api(r.path, { method: "POST" })).json();
    toast("Saved as a new meme");
    $("#item-dialog").close();
    await loadItems();
    openItem(created.id);
  } catch (err) {
    toast(`Save failed: ${err.message}`);
  }
});

// ---------- start ----------

async function start() {
  try {
    const cfg = await (await api("/api/config")).json();
    state.presets = cfg.presets;
    state.captions = cfg.captions;
    await loadItems();
  } catch (err) {
    toast(`Could not load: ${err.message}`, 8000);
    return;
  }
  // Files shared in from the system share sheet land here.
  const q = new URLSearchParams(location.search);
  if (q.has("added") || q.has("failed")) {
    if (q.get("failed")) toast(`Some files failed: ${q.get("failed")}`, 7000);
    else toast(`Added ${q.get("added")} meme${q.get("added") === "1" ? "" : "s"}`);
    if (q.get("open")) openItem(q.get("open"));
    history.replaceState(null, "", "/");
  }
}

if ("serviceWorker" in navigator) navigator.serviceWorker.register("/sw.js").catch(() => {});
start();
