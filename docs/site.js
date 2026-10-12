import { mount } from "./mount.js";
import * as piece from "./whorl-knot.js";

const stage = document.querySelector(".knot");
const pre = stage.querySelector("pre");

if (!matchMedia("(prefers-reduced-motion: reduce)").matches) {
  const { cols, rows, cell = 2 } = piece.meta;
  const canvas = document.createElement("canvas");
  canvas.setAttribute("role", "img");
  canvas.setAttribute("aria-label", pre.getAttribute("aria-label"));
  pre.setAttribute("aria-hidden", "true");
  // mount sizes a canvas by its width and only fills an empty inline style;
  // the width is set here, from the stage's height, so the piece fits the viewport
  const size = () => {
    canvas.style.width = Math.min(stage.clientWidth, (stage.clientHeight * cols) / (rows * cell)) + "px";
  };
  stage.append(canvas);
  size();
  mount(canvas, piece);
  new ResizeObserver(size).observe(stage);
}

for (const btn of document.querySelectorAll(".copy")) {
  btn.addEventListener("click", () => {
    copy(btn.dataset.copy).then((ok) => {
      // the label is "[copy ..."; the status swaps the verb in place
      const rest = btn.dataset.label.slice(5);
      btn.textContent = ok ? "[copied" + rest : "[failed" + rest;
      clearTimeout(btn.t);
      btn.t = setTimeout(() => { btn.textContent = btn.dataset.label; }, 1500);
    });
  });
}

async function copy(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch {
    const ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.append(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch {}
    ta.remove();
    return ok;
  }
}
