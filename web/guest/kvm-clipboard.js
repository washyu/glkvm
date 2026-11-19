/*
 * Clipboard sync for the GL KVM web UI, through glkvm-agent on the host.
 *
 * Loaded into GL's (closed, prebuilt) UI by a <script> tag in
 * /usr/share/kvmd/glweb/index.html (see contrib/rm1-acm/S51guest-ui).
 * It doesn't touch GL's code. It listens in the capture phase, ahead of GL's
 * own keyboard handler on the video container:
 *
 *   Paste:  Ctrl+V (Cmd+V on a Mac) on the video first puts this device's
 *           clipboard on the host, then lets the keystroke through, so the
 *           host pastes the right thing. Keys pressed meanwhile are held and
 *           replayed in order.
 *   Copy:   when the host clipboard changes, the text is written to this
 *           device's clipboard while the tab has focus.
 *
 * Text only. Uses the kvmd API: GET/POST /api/guest/clipboard, GET /api/guest.
 * Click the small pill at the bottom left to turn sync off or on.
 */

(() => {
	"use strict";
	if (window.__glkvmClipboard) return;
	window.__glkvmClipboard = true;

	const STORE_KEY = "glkvm-clipboard-sync";
	const POLL_MS = 1000;
	const PASTE_TIMEOUT_MS = 1500;
	const isMac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);

	let enabled = true;
	try { enabled = (localStorage.getItem(STORE_KEY) !== "off"); } catch (_) { /* storage blocked */ }

	let supported = false;   // The agent has the clipboard commands and is connected
	let hostSeq;             // undefined until the first read: don't copy what was there before
	let lastPushed = null;   // Text we put on the host; don't copy it straight back
	let pendingLocal = null; // Host text a browser refused to write without a user gesture

	// ===== UI: a small pill and toasts

	const pill = document.createElement("div");
	const toast = document.createElement("div");
	const css = (el, style) => Object.assign(el.style, style);
	css(pill, {
		position: "fixed", left: "8px", bottom: "8px", zIndex: 2147483000, padding: "2px 8px",
		font: "11px/18px system-ui, sans-serif", borderRadius: "9px", cursor: "pointer", userSelect: "none",
		background: "rgba(0,0,0,.55)", color: "#fff", opacity: ".55", display: "none",
	});
	css(toast, {
		position: "fixed", left: "50%", bottom: "40px", transform: "translateX(-50%)", zIndex: 2147483001,
		padding: "6px 12px", font: "13px/1.4 system-ui, sans-serif", borderRadius: "6px",
		background: "rgba(0,0,0,.8)", color: "#fff", pointerEvents: "none", display: "none", maxWidth: "80vw",
	});
	pill.onmouseenter = () => { pill.style.opacity = "1"; };
	pill.onmouseleave = () => { pill.style.opacity = ".55"; };
	pill.onclick = () => {
		enabled = !enabled;
		try { localStorage.setItem(STORE_KEY, enabled ? "on" : "off"); } catch (_) { /* ignore */ }
		renderPill();
		showToast(enabled ? "Clipboard sync on" : "Clipboard sync off");
	};

	function renderPill() {
		pill.style.display = supported ? "block" : "none";
		pill.textContent = enabled ? "\u{1F4CB} sync on" : "\u{1F4CB} sync off";
		pill.title = enabled
			? "Clipboard sync with the host is on: Ctrl+V on the video pastes this device's clipboard, and text copied on the host lands here. Click to turn off."
			: "Clipboard sync with the host is off. Click to turn on.";
	}

	let toastTimer = null;
	function showToast(text, ms = 1800) {
		toast.textContent = text;
		toast.style.display = "block";
		clearTimeout(toastTimer);
		toastTimer = setTimeout(() => { toast.style.display = "none"; }, ms);
	}

	function mountUi() {
		if (document.body && !pill.isConnected) {
			document.body.append(pill, toast);
			renderPill();
		}
	}

	// ===== API

	async function api(method, path, json) {
		const opts = {method, credentials: "same-origin", cache: "no-store"};
		if (json !== undefined) {
			opts.headers = {"Content-Type": "application/json"};
			opts.body = JSON.stringify(json);
		}
		const resp = await fetch(`/api${path}`, opts);
		let body = null;
		try { body = await resp.json(); } catch (_) { /* not JSON */ }
		if (!resp.ok || !body || body.ok === false) {
			const err = new Error(body?.result?.error_msg || `HTTP ${resp.status}`);
			err.status = resp.status;
			throw err;
		}
		return body.result;
	}

	// ===== Host -> this device

	async function writeLocal(text) {
		try {
			await navigator.clipboard.writeText(text);
			pendingLocal = null;
			showToast(`Copied from host (${text.length} chars)`);
		} catch (_) {
			// Firefox and Safari need a user gesture; retry on the next key or click
			pendingLocal = text;
		}
	}

	async function pollHost() {
		if (!enabled || !supported || document.hidden) return;
		let clip;
		try {
			clip = await api("GET", "/guest/clipboard");
		} catch (_) {
			return;
		}
		if (clip.seq === hostSeq) return;
		const first = (hostSeq === undefined);
		hostSeq = clip.seq;
		if (first || clip.text == null) return;
		if (clip.text === lastPushed) {
			lastPushed = null; // Our own paste coming back
			return;
		}
		if (document.hasFocus()) {
			await writeLocal(clip.text);
		} else {
			pendingLocal = clip.text;
		}
	}

	async function checkSupport() {
		if (/^#\/(login|init|twofactor)/.test(location.hash)) {
			supported = false; // Not logged in: don't add 403s to GL's login page
			renderPill();
			return;
		}
		try {
			const state = await api("GET", "/guest");
			supported = Boolean(state.connected && state.clipboard?.supported);
		} catch (_) {
			supported = false; // Not logged in yet, or no guest API
		}
		mountUi();
		renderPill();
	}

	// ===== This device -> host (on paste)

	const synthetic = new WeakSet();
	let holding = null; // {queue: [...]} while a paste is being pushed to the host

	function isEditable(el) {
		if (!el || el === document.body) return false;
		if (el.isContentEditable) return true;
		const tag = el.tagName;
		return tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT";
	}

	function onVideo(target) {
		// GL captures keys on the stream container, which holds the <video>
		return Boolean(target) && target !== document.body && !isEditable(target)
			&& Boolean(target.querySelector?.("video") || target.closest?.("video"));
	}

	function isPasteKey(ev) {
		// key covers non-QWERTY layouts, where browsers also paste on the "v" character
		return ev.type === "keydown" && (ev.code === "KeyV" || ev.key?.toLowerCase() === "v")
			&& !ev.altKey && !ev.shiftKey
			&& (isMac ? ev.metaKey : ev.ctrlKey);
	}

	function replay(queue) {
		for (const item of queue) {
			const ev = new KeyboardEvent(item.type, item.init);
			synthetic.add(ev);
			item.target.dispatchEvent(ev);
		}
	}

	function hold(ev, allowDefault = false) {
		const {type, key, code, location, repeat, ctrlKey, shiftKey, altKey, metaKey} = ev;
		holding.queue.push({
			type, target: ev.target,
			init: {key, code, location, repeat, ctrlKey, shiftKey, altKey, metaKey, bubbles: true, cancelable: true},
		});
		ev.stopImmediatePropagation(); // GL never sees it; we replay it later
		if (!allowDefault) ev.preventDefault();
	}

	async function pushToHost(text) {
		if (!text) return; // Nothing readable: the host pastes its own clipboard
		await api("POST", "/guest/clipboard", {text});
		lastPushed = text.replace(/\r\n/g, "\n");
		showToast(`Pasted to host (${text.length} chars)`);
	}

	function startPaste(ev) {
		holding = {queue: []};
		// Keep the default action: on a non-editable element it just fires the
		// browser's "paste" event, which carries the clipboard text without any
		// permission prompt.
		hold(ev, true);
		const state = holding;
		let done = false;
		let gotPasteEvent = null;

		const finish = () => {
			if (done) return;
			done = true;
			document.removeEventListener("paste", onPaste, true);
			holding = null;
			replay(state.queue);
		};
		const onPaste = (pev) => {
			gotPasteEvent = pev.clipboardData?.getData("text/plain") ?? null;
			pev.preventDefault();
			pev.stopImmediatePropagation();
		};

		document.addEventListener("paste", onPaste, true);

		const timer = setTimeout(() => {
			showToast("Clipboard took too long; pasting the host's own clipboard");
			finish();
		}, PASTE_TIMEOUT_MS);

		(async () => {
			await new Promise((resolve) => setTimeout(resolve, 30)); // Let the paste event fire
			let text = gotPasteEvent;
			if (text == null && navigator.clipboard?.readText) {
				try {
					text = await navigator.clipboard.readText();
				} catch (_) {
					showToast("Allow clipboard access for this site to paste into the host", 3500);
				}
			}
			try {
				await pushToHost(text);
			} catch (err) {
				showToast(`Paste to host failed: ${err.message}`, 3500);
			}
			clearTimeout(timer);
			finish();
		})();
	}

	function onKey(ev) {
		if (synthetic.has(ev)) return;
		if (pendingLocal !== null && document.hasFocus()) {
			writeLocal(pendingLocal); // Inside a user gesture now
		}
		if (holding) {
			hold(ev);
			return;
		}
		if (enabled && supported && isPasteKey(ev) && onVideo(ev.target)) {
			startPaste(ev);
		}
	}

	window.addEventListener("keydown", onKey, true);
	window.addEventListener("keyup", onKey, true);
	window.addEventListener("pointerdown", () => {
		if (pendingLocal !== null) writeLocal(pendingLocal);
	}, true);
	window.addEventListener("focus", () => {
		if (pendingLocal !== null) writeLocal(pendingLocal);
		pollHost();
	});

	// ===== Start

	checkSupport();
	setInterval(checkSupport, 10000);
	setInterval(pollHost, POLL_MS);
})();
