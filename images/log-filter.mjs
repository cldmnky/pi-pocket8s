#!/usr/bin/env node
/**
 * Runs the Pi Pocket launcher and keeps the owner sign-in token out of the container log.
 *
 * The launcher prints the owner's sign-in link (and a QR code that encodes it) to its output;
 * whoever reads the log could otherwise sign in as the owner. This wrapper replaces the token
 * in `/login?token=...` with `[redacted]` and hides QR blocks, while every other line,
 * including errors, passes through unchanged. The token itself stays in `<data>/config.json`
 * (mode 0600), which is how the owner retrieves it (for example with `oc exec`).
 *
 * The container runs this as PID 1: signals are forwarded to the launcher and the launcher's
 * exit code becomes the container's.
 */
import { spawn } from "node:child_process";
import { constants } from "node:os";
import { createInterface } from "node:readline";

const LAUNCHER = "/opt/pi-pocket/bin/pi-pocket.js";
const LAUNCHER_ARGS = ["--disable-warning=ExperimentalWarning", LAUNCHER, ...process.argv.slice(2)];

/** The sign-in token is base64url; stop at anything that is not part of it. */
const SIGN_IN_TOKEN = /(\/login\?token=)[A-Za-z0-9._~-]+/g;

/** Terminal QR codes are drawn with block elements (U+2580–U+259F). */
const QR_BLOCK = /[\u2580-\u259f]/;

const QR_NOTICE =
    "  [QR code hidden: it encodes the owner sign-in token; read the token from ~/.pi-pocket/config.json]";

let qrNoted = false;

function redact(line) {
    if (QR_BLOCK.test(line)) {
        if (qrNoted) {
            return undefined;
        }

        qrNoted = true;

        return QR_NOTICE;
    }

    return line.replace(SIGN_IN_TOKEN, "$1[redacted]");
}

function pipe(stream, write) {
    createInterface({ input: stream }).on("line", (line) => {
        const redacted = redact(line);

        if (redacted !== undefined) {
            write(`${redacted}\n`);
        }
    });
}

const child = spawn(process.execPath, LAUNCHER_ARGS, { stdio: ["inherit", "pipe", "pipe"] });

pipe(child.stdout, (line) => process.stdout.write(line));
pipe(child.stderr, (line) => process.stderr.write(line));

for (const signal of ["SIGTERM", "SIGINT", "SIGHUP", "SIGUSR2"]) {
    process.on(signal, () => {
        if (child.exitCode === null && child.signalCode === null) {
            child.kill(signal);
        } else {
            process.exit(0);
        }
    });
}

child.on("error", (error) => {
    console.error(`[pi-pocket-log-filter] could not run the launcher: ${error.message}`);
    process.exit(1);
});

child.on("exit", (code, signal) => {
    if (code !== null) {
        process.exit(code);
    }

    const number = signal === null ? 1 : (constants.signals[signal] ?? 0);

    process.exit(number === 0 ? 1 : 128 + number);
});
