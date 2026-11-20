#!/usr/bin/env python3
"""End-to-end test: CLIProxyAPI v8 + cpa-stego-guard + a fake api.anthropic.com.

Run it through scripts/e2e.sh, which builds the plugin and starts this script
inside a private mount and network namespace (unshare -mn). Inside it:

* the loopback interface is brought up and /etc/hosts is bind-mounted so that
  api.anthropic.com resolves to 127.0.0.1, where scripts/fake_anthropic.py
  serves HTTPS with a throwaway CA that only the CLIProxyAPI process trusts;
* CLIProxyAPI runs with a real-looking Claude OAuth credential, so the request
  takes the OAuth path, including CCH signing, exactly as in production;
* nothing outside the namespace is reachable or modified.
"""

import argparse
import base64
import hashlib
import json
import os
import re
import shutil
import socket
import struct
import fcntl
import subprocess
import sys
import time
import urllib.error
import urllib.request

CPA_PORT = 18317
API_KEY = "e2e-downstream-key"
SESSION_ID = "6f9d2c64-1b9f-4a5e-9d55-0c3f6e1b2a77"
DEVICE_ID = "a" * 64
ACCOUNT_UUID = "0b8f4c2e-5d4a-4c1e-9f7a-2e6d8c9b1a30"

Q_DOMAIN, Q_LAB, Q_BOTH = "\u2019", "\u02bc", "\u02b9"
SYSTEM_PROMPT = "You are Claude Code, Anthropic\u2019s official CLI for Claude. If it\u2019s unclear, ask."
REMINDER = ("<system-reminder>\nAs you answer the user's questions, you can use the following context:\n"
            "# claudeMd\nCodebase and user instructions are shown below.\n\n# currentDate\n{date}\n\n"
            "      IMPORTANT: this context may or may not be relevant to your tasks. "
            "You should not respond to this context unless it is highly relevant to your task.\n</system-reminder>\n")

TARGET_VERSION = "2.1.321"  # CPA v8's built-in baseline (claude-cli/2.1.321)
USER_TEXT = "Fix the failing test."

results = []


def fingerprint(text, version):
    """Claude Code's cc_version fingerprint, computed independently of the plugin."""
    units = text.encode("utf-16-le")
    sampled = b"".join(units[i * 2:i * 2 + 2] if i * 2 + 2 <= len(units) else b"0\x00" for i in (4, 7, 20))
    picked = sampled.decode("utf-16-le", errors="replace")
    return hashlib.sha256(("59cf53e54c78" + picked + version).encode("utf-8")).hexdigest()[:3]


def cc_version_of(billing_text):
    match = re.search(r"cc_version=([0-9.]+)\.([0-9a-f]{3});", billing_text)
    return (match.group(1), match.group(2)) if match else (None, None)


def signed_cch(billing_text):
    match = re.search(r" cch=([0-9a-f]{5});", billing_text)
    return match is not None and match.group(1) != "00000"


def check(name, condition, detail=""):
    results.append((name, bool(condition), detail))
    mark = "PASS" if condition else "FAIL"
    print(f"  [{mark}] {name}" + (f" -- {detail}" if detail and not condition else ""), flush=True)


def loopback_up():
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    ifr = struct.pack("16sH", b"lo", 0)
    flags = struct.unpack("16sH", fcntl.ioctl(s, 0x8913, ifr)[:18])[1]
    fcntl.ioctl(s, 0x8914, struct.pack("16sH", b"lo", flags | 1))


def date_line(quote="'", sep="-"):
    return f"Today{quote}s date is 2026{sep}10{sep}09."


def cc_body(date, user_text=USER_TEXT, stream=True, cc_version="2.1.321.a1b"):
    body = {
        "model": "claude-sonnet-4-6",
        "messages": [{"role": "user", "content": [
            {"type": "text", "text": REMINDER.format(date=date)},
            {"type": "text", "text": user_text, "cache_control": {"type": "ephemeral"}},
        ]}],
        "system": [
            {"type": "text", "text": "x-anthropic-billing-header: cc_version=" + cc_version + "; cc_entrypoint=cli;"},
            {"type": "text", "text": SYSTEM_PROMPT, "cache_control": {"type": "ephemeral"}},
        ],
        "tools": [{"name": "Bash", "description": "Run a shell command.",
                   "input_schema": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}],
        "metadata": {"user_id": json.dumps({"device_id": DEVICE_ID, "account_uuid": ACCOUNT_UUID, "session_id": SESSION_ID},
                                           separators=(",", ":"))},
        "max_tokens": 32000,
        "stream": stream,
    }
    # Node's JSON.stringify: compact, non-ASCII kept as UTF-8.
    return json.dumps(body, ensure_ascii=False, separators=(",", ":")).encode("utf-8")


def cc_headers(version):
    return {
        "Accept": "application/json",
        "Content-Type": "application/json",
        "User-Agent": f"claude-cli/{version} (external, cli)",
        "X-Claude-Code-Session-Id": SESSION_ID,
        "X-Stainless-Arch": "arm64",
        "X-Stainless-Lang": "js",
        "X-Stainless-OS": "MacOS",
        "X-Stainless-Package-Version": "0.128.0",
        "X-Stainless-Retry-Count": "0",
        "X-Stainless-Runtime": "node",
        "X-Stainless-Runtime-Version": "v26.12.1",
        "X-Stainless-Timeout": "600",
        "anthropic-beta": "claude-code-20250219,interleaved-thinking-2025-05-14,thinking-token-count-2026-05-13,"
                          "context-management-2025-06-27,prompt-caching-scope-2026-01-05",
        "anthropic-dangerous-direct-browser-access": "true",
        "anthropic-version": "2023-06-01",
        "x-app": "cli",
        "x-api-key": API_KEY,
        "Accept-Encoding": "identity",
    }


class Upstream:
    def __init__(self, workdir, script_dir):
        self.capture = os.path.join(workdir, "capture.jsonl")
        open(self.capture, "w").close()
        self.proc = subprocess.Popen(
            [sys.executable, os.path.join(script_dir, "fake_anthropic.py"), "--port", "443",
             "--cert", os.path.join(workdir, "server.pem"), "--key", os.path.join(workdir, "server.key"),
             "--capture", self.capture],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
        line = self.proc.stdout.readline()
        if "listening" not in line:
            raise SystemExit("fake upstream failed: " + line)

    def entries(self):
        with open(self.capture, encoding="utf-8") as handle:
            return [json.loads(line) for line in handle if line.strip()]

    def stop(self):
        self.proc.terminate()
        self.proc.wait()


class CPA:
    def __init__(self, binary, workdir, name, plugin_config, extra_config=()):
        self.binary, self.workdir, self.name = binary, workdir, name
        self.extra_config = list(extra_config)
        self.config_path = os.path.join(workdir, "config.yaml")
        self.log_path = os.path.join(workdir, f"cpa-{name}.log")
        self.write_config(plugin_config)
        env = {k: v for k, v in os.environ.items() if "proxy" not in k.lower()}
        env["SSL_CERT_FILE"] = os.path.join(workdir, "ca.pem")
        env["HOME"] = workdir
        self.log = open(self.log_path, "w")
        self.proc = subprocess.Popen([binary, "-config", self.config_path], cwd=workdir, env=env,
                                     stdout=self.log, stderr=subprocess.STDOUT)
        self.wait_for("cpa-stego-guard configured", 60)
        # Requests sent before the credential's models are registered fail
        # with "unknown provider".
        self.wait_for("full client load complete", 60)
        self.wait_http()

    def write_config(self, plugin_config):
        lines = [
            'host: "127.0.0.1"', f"port: {CPA_PORT}", f'auth-dir: "{self.workdir}/auth"',
            "api-keys:", f'  - "{API_KEY}"', "debug: true", "logging-to-file: false",
            "remote-management:", "  allow-remote: false", '  secret-key: ""', "  disable-control-panel: true",
            "plugins:", "  enabled: true", f'  dir: "{self.workdir}/plugins"', "  configs:",
            "    cpa-stego-guard:", "      enabled: true",
        ]
        lines += [f"      {k}: {v}" for k, v in plugin_config.items()]
        lines += self.extra_config
        with open(self.config_path, "w") as handle:
            handle.write("\n".join(lines) + "\n")

    def log_text(self):
        with open(self.log_path, encoding="utf-8", errors="replace") as handle:
            return handle.read()

    def wait_for(self, needle, timeout, after=0):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if self.proc.poll() is not None:
                raise SystemExit(f"CPA exited early, log:\n{self.log_text()[-4000:]}")
            if needle in self.log_text()[after:]:
                return
            time.sleep(0.2)
        raise SystemExit(f"timeout waiting for {needle!r}, log:\n{self.log_text()[-4000:]}")

    def wait_http(self):
        for _ in range(100):
            try:
                urllib.request.urlopen(f"http://127.0.0.1:{CPA_PORT}/healthz", timeout=1)
                return
            except Exception:
                time.sleep(0.2)
        raise SystemExit("CPA did not answer /healthz")

    def stop(self):
        self.proc.terminate()
        try:
            self.proc.wait(10)
        except subprocess.TimeoutExpired:
            self.proc.kill()
        self.log.close()


def post(path, body, headers):
    req = urllib.request.Request(f"http://127.0.0.1:{CPA_PORT}{path}", data=body, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            return resp.status, resp.read()
    except urllib.error.HTTPError as err:
        return err.code, err.read()


def send(upstream, path, body, version="2.1.321", headers=None):
    before = len(upstream.entries())
    status, raw = post(path, body, headers or cc_headers(version))
    entries = upstream.entries()
    sent = [base64.b64decode(e["body_b64"]) for e in entries[before:]]
    heads = [dict(e["headers"]) for e in entries[before:]]
    return status, raw, sent, heads


def first_user_text(body):
    doc = json.loads(body)
    for message in doc.get("messages", []):
        if message.get("role") == "user":
            content = message["content"]
            if isinstance(content, str):
                return content
            return "\n".join(block.get("text", "") for block in content if block.get("type") == "text")
    return ""


def billing(body):
    system = json.loads(body).get("system") or []
    return system[0]["text"] if system and isinstance(system, list) else ""


def describe(body, heads):
    doc = json.loads(body)
    system = doc.get("system") or []
    return {
        "system_blocks": len(system) if isinstance(system, list) else "string",
        "original_prompt_kept": any(isinstance(b, dict) and b.get("text") == SYSTEM_PROMPT for b in system) if isinstance(system, list) else False,
        "first_user_blocks": len(doc["messages"][0]["content"]) if isinstance(doc["messages"][0]["content"], list) else "string",
        "date_reminders": first_user_text(body).count("# currentDate"),
        "billing": billing(body),
        "user_agent": heads.get("User-Agent", ""),
    }


def run(args):
    script_dir = os.path.dirname(os.path.abspath(__file__))
    workdir = args.workdir
    loopback_up()
    subprocess.run(["mount", "--bind", os.path.join(workdir, "hosts"), "/etc/hosts"], check=True)
    upstream = Upstream(workdir, script_dir)
    marked = cc_body(date_line(Q_DOMAIN, "/"))
    clean = cc_body(date_line())
    summary = {}
    try:
        # ---- mode: off (baseline: CPA alone) ----
        print("\n==> mode=off (CPA without cleaning, baseline)", flush=True)
        cpa = CPA(args.cpa, workdir, "off", {"mode": "off"})
        status, raw, sent, heads = send(upstream, "/v1/messages", clean)
        check("off: clean request forwarded", status == 200 and len(sent) == 1, f"status={status} body={raw[:300]!r}")
        if not sent:
            raise SystemExit(1)
        baseline_clean = sent[0]
        summary["confirmed_clean_upstream"] = describe(sent[0], heads[0])
        status, _, sent, _ = send(upstream, "/v1/messages", marked)
        check("off: marked request reaches upstream with markers intact",
              status == 200 and date_line(Q_DOMAIN, "/") in first_user_text(sent[0]))
        status, _, sent, heads = send(upstream, "/v1/messages", marked, version="2.1.150")
        summary["old_client_off_upstream"] = describe(sent[0], heads[0])
        summary["old_client_off_first_user"] = first_user_text(sent[0])
        cpa.stop()

        # ---- mode: clean (default) ----
        print("\n==> mode=clean (default)", flush=True)
        cpa = CPA(args.cpa, workdir, "clean", {"mode": "clean"})
        status, _, sent, heads = send(upstream, "/v1/messages", clean)
        check("clean: clean request is byte-identical upstream to mode=off", status == 200 and sent[0] == baseline_clean)
        for label, quote, sep in [("U+2019", Q_DOMAIN, "-"), ("U+02BC", Q_LAB, "-"), ("U+02B9", Q_BOTH, "-"),
                                  ("slash date", "'", "/"), ("U+2019 + slash", Q_DOMAIN, "/"), ("U+02B9 + slash", Q_BOTH, "/")]:
            status, _, sent, heads = send(upstream, "/v1/messages", cc_body(date_line(quote, sep)))
            ok = status == 200 and len(sent) == 1
            check(f"clean: {label} -> upstream body identical to the clean request (incl. cch)",
                  ok and sent[0] == baseline_clean,
                  f"status={status} billing={billing(sent[0]) if sent else ''} base={billing(baseline_clean)}")
        up = sent[0]
        check("clean: system prompt U+2019 untouched", SYSTEM_PROMPT in up.decode("utf-8"))
        check("clean: upstream billing header carries a signed cch", re.search(r"cch=[0-9a-f]{5};", billing(up)) is not None
              and "cch=00000;" not in billing(up), billing(up))
        summary["confirmed_cleaned_upstream"] = describe(up, heads[0])
        check("clean: CPA treats the request as confirmed Claude Code (system prompt passed through, 2 blocks)",
              summary["confirmed_cleaned_upstream"]["original_prompt_kept"] and summary["confirmed_cleaned_upstream"]["system_blocks"] == 2,
              json.dumps(summary["confirmed_cleaned_upstream"], ensure_ascii=False))
        check("clean: upstream view equals mode=off view of a clean request",
              summary["confirmed_cleaned_upstream"] == summary["confirmed_clean_upstream"])

        stray = cc_body(date_line(), user_text="Hawai\u02bbi \u02bc test \u02b9")
        status, _, sent, _ = send(upstream, "/v1/messages", stray)
        check("clean: U+02BC/U+02B9 outside the date line are forwarded unchanged",
              status == 200 and "Hawai\u02bbi \u02bc test \u02b9" in sent[0].decode("utf-8"))
        status, _, sent, _ = send(upstream, "/v1/messages", cc_body(date_line(Q_LAB, "/"), stream=False))
        check("clean: non-streaming request cleaned", status == 200 and date_line() in first_user_text(sent[0]))
        status, _, sent, _ = send(upstream, "/v1/messages/count_tokens", cc_body(date_line(Q_DOMAIN, "/"), stream=False))
        check("clean: count_tokens request cleaned", status == 200 and sent and date_line() in first_user_text(sent[0]),
              f"status={status} sent={len(sent)}")
        status, _, sent, heads = send(upstream, "/v1/messages", marked, version="2.1.150")
        check("clean: old client (2.1.150, not confirmed by CPA) cleaned too",
              status == 200 and date_line(Q_DOMAIN, "/") not in sent[0].decode("utf-8"))
        summary["old_client_clean_upstream"] = describe(sent[0], heads[0])
        summary["old_client_clean_first_user"] = first_user_text(sent[0])
        log = cpa.log_text()
        check("clean: host log records the hit with counts", "cpa-stego-guard found steganographic markers" in log
              and "action=cleaned" in log and "quote_u2019_domain=1" in log, "")
        check("clean: host log has no prompt text", "Fix the failing test" not in log and "Hawai" not in log)

        # Hot reconfigure to block: the host re-registers and the hook switches.
        print("\n==> reconfigure clean -> block without restart", flush=True)
        offset = len(cpa.log_text())
        cpa.write_config({"mode": "block"})
        cpa.wait_for("hook=request.intercept_before", 30, after=offset)
        time.sleep(1)
        status, raw, sent, _ = send(upstream, "/v1/messages", marked)
        check("reconfigure: marked request now blocked with 400", status == 400 and not sent, f"status={status}")
        cpa.stop()

        # ---- mode: block ----
        print("\n==> mode=block", flush=True)
        cpa = CPA(args.cpa, workdir, "block", {"mode": "block"})
        for label, body in [("U+2019", cc_body(date_line(Q_DOMAIN))), ("slash", cc_body(date_line("'", "/"))),
                            ("U+02B9 + slash", cc_body(date_line(Q_BOTH, "/")))]:
            status, raw, sent, _ = send(upstream, "/v1/messages", body)
            check(f"block: {label} -> 400, nothing sent upstream", status == 400 and not sent, f"status={status}")
        error = json.loads(raw)
        summary["block_response"] = error
        check("block: Anthropic error body names markers and the fix",
              error.get("type") == "error" and error["error"]["type"] == "invalid_request_error"
              and "U+02B9" in error["error"]["message"] and "YYYY/MM/DD" in error["error"]["message"]
              and "2.1.293" in error["error"]["message"])
        status, _, sent, _ = send(upstream, "/v1/messages", clean)
        check("block: clean request forwarded byte-identical", status == 200 and sent and sent[0] == baseline_clean)
        status, _, sent, _ = send(upstream, "/v1/messages", stray)
        check("block: stray U+02BC only -> forwarded", status == 200 and len(sent) == 1)
        check("block: host log warns on block", "action=blocked" in cpa.log_text())
        cpa.stop()

        print("\n==> mode=block, clean_region_marker=false", flush=True)
        cpa = CPA(args.cpa, workdir, "block-noregion", {"mode": "block", "clean_region_marker": "false"})
        status, _, sent, _ = send(upstream, "/v1/messages", cc_body(date_line("'", "/")))
        check("block/noregion: slash only -> forwarded unchanged", status == 200 and sent
              and date_line("'", "/") in first_user_text(sent[0]))
        status, _, sent, _ = send(upstream, "/v1/messages", cc_body(date_line(Q_DOMAIN, "/")))
        check("block/noregion: U+2019 still blocked", status == 400 and not sent)
        cpa.stop()

        print("\n==> mode=clean, clean_quote_markers=false", flush=True)
        cpa = CPA(args.cpa, workdir, "clean-noquote", {"mode": "clean", "clean_quote_markers": "false"})
        status, _, sent, _ = send(upstream, "/v1/messages", cc_body(date_line(Q_DOMAIN, "/")))
        check("clean/noquote: only the slash is cleaned", status == 200 and date_line(Q_DOMAIN, "-") in first_user_text(sent[0]))
        cpa.stop()

        # ---- cc_version alignment with stabilize-device-profile ----
        newer_headers = cc_headers("2.1.330")
        newer_headers["X-Stainless-Runtime-Version"] = "v26.14.0"
        newer_headers["X-Stainless-OS"] = "Windows"
        newer_headers["X-Stainless-Arch"] = "x64"
        fp_client = fingerprint(USER_TEXT, "2.1.330")
        fp_target = fingerprint(USER_TEXT, TARGET_VERSION)
        newer = cc_body(date_line(), cc_version=f"2.1.330.{fp_client}")
        bad_fp = "000" if fp_client != "000" else "001"
        tampered = cc_body(date_line(), cc_version=f"2.1.330.{bad_fp}")
        stabilize = ["claude-header-defaults:", "  stabilize-device-profile: true"]

        print("\n==> stabilize-device-profile=true, no cc_version (the problem)", flush=True)
        cpa = CPA(args.cpa, workdir, "stabilize-off", {"mode": "clean"}, stabilize)
        check("no cc_version: registration warns that alignment is disabled", "cc_version is empty" in cpa.log_text())
        status, _, sent, heads = send(upstream, "/v1/messages", newer, headers=newer_headers)
        observed = describe(sent[0], heads[0])
        summary["stabilize_without_cc_version"] = observed
        check("no cc_version: CPA sends UA 2.1.321 but the body keeps cc_version 2.1.330 (mismatch)",
              status == 200 and observed["user_agent"].startswith(f"claude-cli/{TARGET_VERSION} ")
              and cc_version_of(observed["billing"]) == ("2.1.330", fp_client), json.dumps(observed))
        cpa.stop()

        print("\n==> stabilize-device-profile=true, cc_version=2.1.321", flush=True)
        cpa = CPA(args.cpa, workdir, "stabilize", {"mode": "clean", "cc_version": TARGET_VERSION}, stabilize)
        check("cc_version: registration logs the target", f"cc_version={TARGET_VERSION}" in cpa.log_text())
        # Reference: a client already at the target version; the plugin leaves it alone.
        reference = cc_body(date_line(), cc_version=f"{TARGET_VERSION}.{fp_target}")
        status, _, sent, heads = send(upstream, "/v1/messages", reference)
        check("cc_version: 2.1.321 client forwarded", status == 200 and len(sent) == 1, f"status={status}")
        reference_up = sent[0]
        summary["stabilize_reference_2_1_321"] = describe(reference_up, heads[0])
        check("cc_version: 2.1.321 client upstream billing is cc_version=2.1.321.<fp> with a signed cch",
              cc_version_of(billing(reference_up)) == (TARGET_VERSION, fp_target) and signed_cch(billing(reference_up)),
              billing(reference_up))

        offset = len(cpa.log_text())
        status, _, sent, heads = send(upstream, "/v1/messages", newer, headers=newer_headers)
        up = sent[0] if sent else b"{}"
        observed = describe(up, heads[0] if heads else {})
        observed.update({k: (heads[0] if heads else {}).get(k, "") for k in ["X-Stainless-Package-Version", "X-Stainless-Runtime-Version"]})
        summary["stabilize_aligned_2_1_330"] = observed
        print("  observed: " + json.dumps(observed), flush=True)
        check("cc_version: 2.1.330 client -> upstream UA claude-cli/2.1.321",
              status == 200 and observed["user_agent"].startswith(f"claude-cli/{TARGET_VERSION} "), observed["user_agent"])
        check("cc_version: 2.1.330 client -> upstream cc_version=2.1.321.<independently computed fingerprint>",
              cc_version_of(observed["billing"]) == (TARGET_VERSION, fp_target), observed["billing"])
        check("cc_version: aligned request is still confirmed Claude Code (system prompt passed through, 2 blocks)",
              observed["original_prompt_kept"] and observed["system_blocks"] == 2, json.dumps(observed))
        check("cc_version: cch valid -- upstream body byte-identical to CPA's signed 2.1.321 reference",
              up == reference_up and signed_cch(observed["billing"]), f"{observed['billing']} vs {billing(reference_up)}")
        check("cc_version: alignment logged at debug with versions only",
              "cpa-stego-guard aligned cc_version" in cpa.log_text()[offset:] and "cc_version_from=2.1.330" in cpa.log_text()[offset:])

        # Idempotent end to end: the aligned body sent again is not changed further.
        status, _, sent, _ = send(upstream, "/v1/messages", newer, headers=newer_headers)
        check("cc_version: repeated request gives the same upstream bytes", status == 200 and sent and sent[0] == reference_up)

        # Date line cleaning and alignment on the same request.
        marked_newer = cc_body(date_line(Q_BOTH, "/"), cc_version=f"2.1.330.{fp_client}")
        status, _, sent, _ = send(upstream, "/v1/messages", marked_newer, headers=newer_headers)
        check("cc_version + date cleaning: upstream body identical to the reference (incl. cch)",
              status == 200 and sent and sent[0] == reference_up, billing(sent[0]) if sent else "")

        # Tampered fingerprint: the plugin removes the billing header and metadata.user_id; CPA cloaks.
        offset = len(cpa.log_text())
        status, _, sent, heads = send(upstream, "/v1/messages", tampered, headers=newer_headers)
        up = sent[0] if sent else b"{}"
        observed = describe(up, heads[0] if heads else {})
        doc = json.loads(up)
        user_id = (doc.get("metadata") or {}).get("user_id", "")
        observed["metadata_user_id_from_client"] = DEVICE_ID in user_id
        observed["upstream_has_metadata_user_id"] = bool(user_id)
        summary["stabilize_tampered_fallback"] = observed
        summary["stabilize_tampered_fallback_first_user"] = first_user_text(up)
        print("  observed: " + json.dumps(observed), flush=True)
        version, fp_up = cc_version_of(observed["billing"])
        check("fallback: tampered request forwarded", status == 200 and len(sent) == 1, f"status={status}")
        check("fallback: upstream billing is CPA's own, cc_version 2.1.321 with a signed cch",
              version == TARGET_VERSION and fp_up is not None and signed_cch(observed["billing"])
              and f"2.1.330.{bad_fp}" not in up.decode("utf-8"), observed["billing"])
        check("fallback: CPA cloaked the request (caller system prompt not kept in system)",
              not observed["original_prompt_kept"], json.dumps(observed))
        check("fallback: client metadata.user_id not forwarded (CPA supplies its own)",
              not observed["metadata_user_id_from_client"], user_id)
        check("fallback: upstream UA claude-cli/2.1.321", observed["user_agent"].startswith(f"claude-cli/{TARGET_VERSION} "))
        check("fallback: logged with reason", "cc_version_reason=fingerprint_mismatch" in cpa.log_text()[offset:])
        check("fallback: upstream view equals CPA's cloak of an unconfirmed old client (mode=off)",
              {k: observed[k] for k in ("system_blocks", "original_prompt_kept", "first_user_blocks")}
              == {k: summary["old_client_off_upstream"][k] for k in ("system_blocks", "original_prompt_kept", "first_user_blocks")},
              json.dumps([observed, summary["old_client_off_upstream"]]))
        cpa.stop()

        print("\n==> mode=block, cc_version=2.1.321", flush=True)
        cpa = CPA(args.cpa, workdir, "block-cc", {"mode": "block", "cc_version": TARGET_VERSION}, stabilize)
        status, _, sent, _ = send(upstream, "/v1/messages", marked_newer, headers=newer_headers)
        check("block/cc_version: marked request still blocked", status == 400 and not sent, f"status={status}")
        status, _, sent, _ = send(upstream, "/v1/messages", newer, headers=newer_headers)
        check("block/cc_version: unmarked request aligned (identical to the reference)",
              status == 200 and sent and sent[0] == reference_up, billing(sent[0]) if sent else "")
        cpa.stop()

        # ---- mode: log ----
        print("\n==> mode=log", flush=True)
        cpa = CPA(args.cpa, workdir, "log", {"mode": "log", "cc_version": TARGET_VERSION}, stabilize)
        status, _, sent, _ = send(upstream, "/v1/messages", marked)
        check("log: marked request forwarded unchanged", status == 200 and date_line(Q_DOMAIN, "/") in first_user_text(sent[0]))
        check("log: host log records action=logged", "action=logged" in cpa.log_text())
        status, _, sent, _ = send(upstream, "/v1/messages", newer, headers=newer_headers)
        check("log/cc_version: cc_version left at 2.1.330, would-align logged",
              status == 200 and cc_version_of(billing(sent[0])) == ("2.1.330", fp_client)
              and "would align cc_version" in cpa.log_text())
        cpa.stop()
    finally:
        upstream.stop()

    with open(os.path.join(workdir, "summary.json"), "w", encoding="utf-8") as handle:
        json.dump(summary, handle, ensure_ascii=False, indent=2)
    failed = [name for name, ok, _ in results if not ok]
    print(f"\n{len(results) - len(failed)}/{len(results)} checks passed; summary in {workdir}/summary.json")
    if failed:
        print("FAILED: " + "; ".join(failed))
        sys.exit(1)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--cpa", required=True)
    parser.add_argument("--workdir", required=True)
    run(parser.parse_args())
