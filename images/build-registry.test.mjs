// The image build must not go back to Docker Hub or the docker daemon: anonymous Docker
// Hub pull limits answered 429/504 for shared runner IPs and failed whole CI runs, so the
// workflow builds with podman and pulls only from registry.access.redhat.com and quay.io.
// `make test` runs this, which is where a regression should be caught, not in a red run.
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";

/** Everything that decides which registry the images come from and how they are built. */
const FILES = ["images/Containerfile", "portal/Containerfile", ".github/workflows/images.yml"];

test("no image is pulled from Docker Hub", () => {
    for (const file of FILES) {
        const lines = readFileSync(file, "utf8").split("\n");

        for (const [index, line] of lines.entries()) {
            assert.doesNotMatch(
                line,
                /docker\.io\/|docker\.com\//,
                `${file}:${index + 1} points at Docker Hub: ${line.trim()}`,
            );
        }
    }
});

test("the images workflow builds with podman, not the docker daemon", () => {
    const workflow = readFileSync(".github/workflows/images.yml", "utf8");

    assert.match(workflow, /podman build/, "the build runs through podman");
    assert.match(workflow, /podman manifest/, "the multi-arch index is assembled with podman");
    assert.doesNotMatch(workflow, /uses: docker\//, "no docker actions (buildx, QEMU helper, login) are used");
    assert.doesNotMatch(workflow, /^\s*(?:sudo\s+)?docker\s+(?:build|run|exec|logs|rm|volume|login|manifest)\b/m,
        "smoke and publish commands must use podman too; auto-merges can reintroduce docker exec");
});

test("the arm64 build runs natively, not under QEMU", () => {
    const workflow = readFileSync(".github/workflows/images.yml", "utf8");

    assert.match(workflow, /ubuntu-24\.04-arm/, "arm64 legs use GitHub's native arm64 runners");
    assert.doesNotMatch(workflow, /qemu-user-static/, "no emulation layer is installed");
});
