import assert from "node:assert/strict";
import test from "node:test";

import { Sandbox, MountFailedException } from "../dist/index.js";

/**
 * Build a Sandbox instance that skips normal construction and wires only the
 * two collaborators exercised by mount/umount.
 */
function makeSandbox({ commands, files } = {}) {
  const sandbox = Object.create(Sandbox.prototype);
  Object.defineProperty(sandbox, "id", { value: "sbx" });
  Object.defineProperty(sandbox, "commands", { value: commands ?? failCommands });
  Object.defineProperty(sandbox, "files", { value: files ?? failFiles });
  return sandbox;
}

const failCommands = {
  run() { throw new Error("commands.run should not be called"); },
};
const failFiles = {
  writeFiles() { throw new Error("files.writeFiles should not be called"); },
};

function okExecution() {
  return {
    id: "e-1",
    logs: { stdout: [], stderr: [] },
    result: [],
    exitCode: 0,
  };
}

function badExecution() {
  return {
    id: "e-2",
    logs: { stdout: [], stderr: [{ text: "boom", timestamp: 0 }] },
    result: [],
    exitCode: 1,
    error: { name: "MountError", value: "denied", timestamp: 0, traceback: [] },
  };
}

// -------- NFS --------

test("sandbox.mount(NfsMountOptions) issues a single mount -t nfs command", async () => {
  const calls = [];
  const commands = {
    async run(cmd) {
      calls.push(cmd);
      return okExecution();
    },
  };
  const files = { async writeFiles() { throw new Error("nfs must not touch filesystem"); } };
  const sandbox = makeSandbox({ commands, files });

  const execution = await sandbox.mount({
    endpoint: "nas",
    nasPath: "/",
    mountPoint: "/mnt/nas",
  });

  assert.equal(execution.exitCode, 0);
  assert.equal(calls.length, 1);
  assert.ok(calls[0].startsWith("mkdir -p '/mnt/nas' && "), calls[0]);
  assert.ok(calls[0].includes("mount -t nfs "), calls[0]);
});

test("sandbox.mount raises MountFailedException on error execution", async () => {
  const commands = { async run() { return badExecution(); } };
  const sandbox = makeSandbox({ commands });
  await assert.rejects(
    () => sandbox.mount({ endpoint: "nas", nasPath: "/", mountPoint: "/mnt/nas" }),
    (err) => err instanceof MountFailedException && /NAS mount failure/.test(err.message),
  );
});

// -------- ossfs 1.x --------

test("sandbox.mount(ossfs 1.0) uploads passwd file before running mount command", async () => {
  const events = [];
  const commands = { async run(cmd) { events.push(["run", cmd]); return okExecution(); } };
  const files = {
    async writeFiles(entries) {
      events.push(["writeFiles", entries]);
    },
  };
  const sandbox = makeSandbox({ commands, files });

  await sandbox.mount({
    endpoint: "e",
    bucket: "b",
    mountPoint: "/mnt/oss",
    accessKeyId: "AK",
    accessKeySecret: "SK",
  });

  assert.equal(events.length, 2);
  assert.equal(events[0][0], "writeFiles", "passwd file must be uploaded first");
  const passwdEntry = events[0][1][0];
  assert.ok(passwdEntry.path.startsWith("/tmp/opensandbox-ossfspass-"), passwdEntry.path);
  assert.equal(passwdEntry.data, "b:AK:SK");
  assert.equal(passwdEntry.mode, 600);

  assert.equal(events[1][0], "run");
  const cmd = events[1][1];
  assert.ok(cmd.includes("ossfs --version"), cmd);
  assert.ok(cmd.includes(`-opasswd_file='${passwdEntry.path}'`), cmd);
  assert.ok(!cmd.includes("AK"), `AK must not leak into shell command, got: ${cmd}`);
  assert.ok(!cmd.includes("SK"), `SK must not leak into shell command, got: ${cmd}`);
});

// -------- ossfs 2.x --------

test("sandbox.mount(ossfs 2.0) uploads conf and env files before running mount command", async () => {
  const events = [];
  const commands = { async run(cmd) { events.push(["run", cmd]); return okExecution(); } };
  const files = {
    async writeFiles(entries) {
      events.push(["writeFiles", entries]);
    },
  };
  const sandbox = makeSandbox({ commands, files });

  await sandbox.mount({
    endpoint: "e",
    bucket: "b",
    mountPoint: "/mnt/oss2",
    accessKeyId: "AK",
    accessKeySecret: "SK",
    version: "2.0",
  });

  assert.equal(events.length, 2);
  assert.equal(events[0][0], "writeFiles", "conf + env must be uploaded first");
  const entries = events[0][1];
  assert.equal(entries.length, 2, "ossfs2 must upload both a conf and an env file");
  const byPath = Object.fromEntries(entries.map((e) => [e.path, e]));
  const confEntry = entries.find((e) => e.path.endsWith(".conf"));
  const envEntry = entries.find((e) => e.path.includes("-ossfsenv-"));
  assert.ok(confEntry && envEntry, JSON.stringify(entries));
  assert.equal(byPath[confEntry.path].mode, 600);
  assert.equal(byPath[envEntry.path].mode, 600);
  assert.ok(envEntry.data.includes("OSS_ACCESS_KEY_ID=AK\n"), envEntry.data);
  assert.ok(envEntry.data.includes("OSS_ACCESS_KEY_SECRET=SK\n"), envEntry.data);

  assert.equal(events[1][0], "run");
  const cmd = events[1][1];
  assert.ok(cmd.includes(`. '${envEntry.path}'`), cmd);
  assert.ok(cmd.includes(`-c '${confEntry.path}'`), cmd);
  assert.ok(!cmd.includes("AK"), `AK must not leak into shell command, got: ${cmd}`);
  assert.ok(!cmd.includes("SK"), `SK must not leak into shell command, got: ${cmd}`);
});

// -------- umount --------

test("sandbox.umount runs umount with quoted path", async () => {
  const calls = [];
  const commands = { async run(cmd) { calls.push(cmd); return okExecution(); } };
  const sandbox = makeSandbox({ commands });
  await sandbox.umount("/mnt/nas");
  assert.deepEqual(calls, ["umount '/mnt/nas'"]);
});

test("sandbox.umount rejects blank path", async () => {
  const sandbox = makeSandbox({});
  await assert.rejects(() => sandbox.umount("   "));
});

// -------- unknown options --------

test("sandbox.mount rejects unknown options type", async () => {
  const sandbox = makeSandbox({});
  await assert.rejects(() => sandbox.mount({}), TypeError);
});
