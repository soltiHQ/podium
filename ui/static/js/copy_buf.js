(() => {
    async function copyText(text) {
        if (!text) return false;

        try {
            await navigator.clipboard.writeText(text);
            return true;
        } catch {
            const ta = document.createElement("textarea");
            ta.value = text;
            ta.style.position = "fixed";
            ta.style.top = "-9999px";
            ta.style.left = "-9999px";
            ta.style.opacity = "0";
            document.body.appendChild(ta);
            ta.focus();
            ta.select();

            try {
                return document.execCommand("copy");
            } catch {
                return false;
            } finally {
                document.body.removeChild(ta);
            }
        }
    }

    function showToast(msg, ok) {
        const t = document.createElement("div");
        t.textContent = msg;
        t.className =
            "fixed bottom-6 left-1/2 -translate-x-1/2 px-4 py-2 rounded-lg " +
            "bg-card border border-border text-sm shadow-md z-50 opacity-0 transition";
        if (!ok) t.className += " text-danger";
        document.body.appendChild(t);

        requestAnimationFrame(() => (t.style.opacity = "1"));
        setTimeout(() => {
            t.style.opacity = "0";
            setTimeout(() => t.remove(), 200);
        }, 1400);
    }

    function flash(el, ok) {
        const prev = el.getAttribute("data-copy-state") || "";
        el.setAttribute("data-copy-state", ok ? "copied" : "copy-failed");
        setTimeout(() => el.setAttribute("data-copy-state", prev), 900);
    }

    document.addEventListener("click", async (e) => {
        const el = e.target.closest("[data-copy]");
        if (!el) return;

        const text =
            el.getAttribute("data-copy-text") ||
            (el.textContent || "").trim();

        const ok = await copyText(text);
        flash(el, ok);
        showToast(ok ? "Copied to buffer" : "Copy failed", ok);
    });
})();