"use strict";

const pad = (n) => String(n).padStart(2, "0");

function format(ms) {
  const s = Math.floor(ms / 1000);
  const d = Math.floor(s / 86400);
  const clock = `${pad(Math.floor(s / 3600) % 24)}:${pad(Math.floor(s / 60) % 60)}:${pad(s % 60)}`;
  return d > 0 ? `${d} j ${clock}` : clock;
}

const counters = [...document.querySelectorAll("[data-tick]")].map((el) => ({ el, base: Number(el.dataset.tick) }));
const loaded = performance.now();
if (counters.length) {
  setInterval(() => {
    const elapsed = performance.now() - loaded;
    for (const c of counters) c.el.textContent = format(c.base + elapsed);
  }, 1000);
}

if (document.querySelector("table.board [data-tick]")) {
  setInterval(() => {
    if (!document.hidden) location.reload();
  }, 60000);
}

for (const block of document.querySelectorAll("pre.code")) {
  const button = document.createElement("button");
  button.type = "button";
  button.className = "copy";
  button.textContent = "Copier";
  button.addEventListener("click", async () => {
    await navigator.clipboard.writeText(block.querySelector("code").textContent);
    button.textContent = "Copié";
    setTimeout(() => (button.textContent = "Copier"), 1500);
  });
  block.append(button);
}
