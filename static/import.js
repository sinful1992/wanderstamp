// F9 import: load a travel-log export back in. app.js calls WSImport.open()
// from the admin family page. It always shows a dry run first, so nothing
// changes until the admin has seen what would come in.
(function () {
  "use strict";
  const style = document.createElement("style");
  style.textContent = `
    .ws-import p { margin: 0 0 12px; }
    .ws-import .note { color: var(--muted); }
    .ws-import button { min-height: 44px; margin: 0 8px 8px 0; }
    .ws-import-sum { list-style: none; padding: 0; margin: 0 0 12px; }
    .ws-import-sum li { padding: 4px 0; }
  `;
  document.head.appendChild(style);

  const plural = (n, one, many = one + "s") => `${n} ${n === 1 ? one : many}`;
  const MAX = 20 * 1024 * 1024;

  function summary(ws, r) {
    const ul = ws.el("ul", "ws-import-sum");
    ul.append(
      ws.el("li", "", plural(r.holidays, "trip")),
      ws.el("li", "", plural(r.pins, "pin")),
      ws.el("li", "", plural(r.photos, "photo")),
      ws.el("li", "", plural(r.packing, "packing item")),
    );
    if (r.skipped) ul.appendChild(ws.el("li", "note", `${plural(r.skipped, "trip")} already here, left as they are`));
    return ul;
  }

  function open() {
    const ws = window.WS;
    const body = ws.openPanel("Import a travel log");
    body.classList.add("ws-import");
    const input = document.createElement("input");
    input.type = "file";
    input.accept = ".json,application/json";
    input.hidden = true;
    const pick = ws.el("button", "primary", "Choose an export file");
    pick.type = "button";
    pick.onclick = () => input.click();
    const out = ws.el("div");
    body.append(
      ws.el("p", "note", "Choose a file saved with Export. Trips already here are skipped, and nothing changes until you confirm."),
      pick, input, out);

    input.onchange = async () => {
      const f = input.files && input.files[0];
      input.value = "";
      out.textContent = "";
      if (!f) return;
      if (f.size > MAX) { out.appendChild(ws.el("p", "", "That file is over 20 MB.")); return; }
      let doc;
      try {
        doc = JSON.parse(await f.text());
      } catch {
        out.appendChild(ws.el("p", "", "That isn't an export file (it isn't JSON)."));
        return;
      }
      if (!doc || !Array.isArray(doc.holidays)) {
        out.appendChild(ws.el("p", "", "That isn't an export file (it has no trips)."));
        return;
      }
      pick.disabled = true;
      let dry;
      try {
        dry = await ws.api("POST", "/api/import?dry_run=1", doc);
      } catch (ex) {
        out.appendChild(ws.el("p", "", `Can't import it: ${ex.message}`));
        pick.disabled = false;
        return;
      }
      pick.disabled = false;
      if (!dry.holidays) {
        out.append(ws.el("p", "", `Nothing new in ${f.name}.`), summary(ws, dry));
        return;
      }
      out.append(ws.el("p", "", `${f.name} would add:`), summary(ws, dry));
      const go = ws.el("button", "primary", `Import ${plural(dry.holidays, "trip")}`);
      go.type = "button";
      const cancel = ws.el("button", "", "Cancel");
      cancel.type = "button";
      cancel.onclick = () => { out.textContent = ""; };
      go.onclick = async () => {
        go.disabled = cancel.disabled = pick.disabled = true;
        go.textContent = "Importing…";
        try {
          const r = await ws.api("POST", "/api/import", doc);
          out.textContent = "";
          out.append(ws.el("p", "", "Imported:"), summary(ws, r));
          ws.toast(`Imported ${plural(r.holidays, "trip")}`);
          // the map picks the new trips up on its next load
          if (typeof ws.reload === "function") ws.reload();
          else setTimeout(() => location.reload(), 1500);
        } catch (ex) {
          go.disabled = cancel.disabled = false;
          go.textContent = `Import ${plural(dry.holidays, "trip")}`;
          ws.toast(`Import failed: ${ex.message}`);
        } finally {
          pick.disabled = false;
        }
      };
      out.append(go, cancel);
    };
  }

  window.WSImport = { open };
})();
