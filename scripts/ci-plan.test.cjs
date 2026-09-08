const { test } = require('node:test');
const assert = require('node:assert/strict');
const { execFileSync } = require('node:child_process');
const { mkdtempSync, mkdirSync, writeFileSync, rmSync } = require('node:fs');
const { tmpdir } = require('node:os');
const { join, dirname } = require('node:path');
const shouldRunChecks = require('./ci-plan.cjs');

function fixture(t) {
  const cwd = mkdtempSync(join(tmpdir(), 'repo-sync-ci-'));
  t.after(() => rmSync(cwd, { recursive: true, force: true }));
  const git = (args) => execFileSync('git', args, {
    cwd, encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'],
    env: { ...process.env, GIT_CONFIG_NOSYSTEM: '1', GIT_CONFIG_GLOBAL: '/dev/null' },
  });
  git(['init', '-q', '--initial-branch=main']);
  git(['config', 'user.name', 'CI test']);
  git(['config', 'user.email', 'ci@example.invalid']);
  const commit = (files) => {
    for (const [file, contents] of Object.entries(files)) {
      if (contents === null) rmSync(join(cwd, file));
      else {
        mkdirSync(dirname(join(cwd, file)), { recursive: true });
        writeFileSync(join(cwd, file), contents);
      }
    }
    git(['add', '--all']);
    git(['-c', 'commit.gpgsign=false', 'commit', '-qm', 'test']);
    return git(['rev-parse', 'HEAD']).trim();
  };
  const base = commit({ 'main.go': 'package main\n', 'README.md': 'hello\n' });
  const run = {
    id: 1, event: 'push', head_branch: 'main', head_sha: base, conclusion: 'success',
    head_repository: { full_name: 'vectal-labs/repo-sync' }, html_url: 'https://example.invalid/run/1',
  };
  const job = {
    name: 'test', head_sha: base, conclusion: 'success',
    steps: [{ name: 'Run go test -race -count=1 ./...', conclusion: 'success' }],
  };
  const calls = [];
  const actions = {
    async listWorkflowRuns(params) { calls.push(params); return { data: { workflow_runs: [run] } }; },
    async listJobsForWorkflowRun(params) { calls.push(params); return { data: { jobs: [job] } }; },
  };
  const context = {
    repo: { owner: 'vectal-labs', repo: 'repo-sync' }, eventName: 'push',
    ref: 'refs/tags/v1.0.0', payload: { before: base },
  };
  const check = () => shouldRunChecks({
    github: { rest: { actions } }, context, git,
    core: { info() {}, warning() {} },
  });
  return { git, commit, base, run, job, actions, context, calls, check };
}

test('release reuses only a fully tested main commit', async (t) => {
  const f = fixture(t);
  assert.equal(await f.check(), false);
  assert.equal(f.calls[0].head_sha, f.base);
  assert.equal(f.calls[0].workflow_id, 'ci.yml');
  assert.equal(f.calls[0].branch, 'main');
  assert.equal(f.calls[0].event, 'push');
  assert.equal(f.calls[0].status, 'success');
  assert.equal(f.calls[1].filter, 'latest');
});

for (const [name, alter] of Object.entries({
  'different commit': (f) => { f.run.head_sha = 'a'.repeat(40); },
  'different repository': (f) => { f.run.head_repository.full_name = 'someone/repo-sync'; },
  'PR run': (f) => { f.run.event = 'pull_request'; },
  'feature branch': (f) => { f.run.head_branch = 'feature'; },
  'failed workflow': (f) => { f.run.conclusion = 'failure'; },
  'failed job': (f) => { f.job.conclusion = 'failure'; },
  'wrong job commit': (f) => { f.job.head_sha = 'b'.repeat(40); },
  'skipped tests': (f) => { f.job.steps[0].conclusion = 'skipped'; },
  'missing tests': (f) => { f.job.steps = []; },
  'no successful run': (f) => { f.actions.listWorkflowRuns = async () => ({ data: { workflow_runs: [] } }); },
  'API failure': (f) => { f.actions.listWorkflowRuns = async () => { throw new Error('unavailable'); }; },
  'job lookup failure': (f) => { f.actions.listJobsForWorkflowRun = async () => { throw new Error('unavailable'); }; },
})) {
  test(`release runs all checks with ${name}`, async (t) => {
    const f = fixture(t);
    alter(f);
    assert.equal(await f.check(), true);
  });
}

test('annotated tag resolves to the checked-out commit', async (t) => {
  const f = fixture(t);
  f.git(['tag', '-a', 'v1.0.0', '-m', 'release']);
  f.context.sha = f.git(['rev-parse', 'v1.0.0']).trim();
  assert.notEqual(f.context.sha, f.base);
  assert.equal(await f.check(), false);
  assert.equal(f.calls[0].head_sha, f.base);
});

test('main skips chains of docs and image changes since full CI', async (t) => {
  const f = fixture(t);
  f.context.ref = 'refs/heads/main';
  f.context.payload.before = f.commit({ 'README.md': 'updated\n' });
  f.commit({ 'docs/image with spaces.webp': 'image', 'docs/guide.md': 'guide' });
  assert.equal(await f.check(), false);
});

test('docs after cancelled code changes still run all checks', async (t) => {
  const f = fixture(t);
  f.context.ref = 'refs/heads/main';
  f.context.payload.before = f.commit({ 'main.go': 'changed code\n' });
  f.commit({ 'README.md': 'updated\n' });
  assert.equal(await f.check(), true);
});

test('a successful docs-only main run cannot authorize a release', async (t) => {
  const f = fixture(t);
  const head = f.commit({ 'README.md': 'updated\n' });
  f.run.head_sha = f.job.head_sha = head;
  f.job.steps[0].conclusion = 'skipped';
  assert.equal(await f.check(), true);
});

test('main docs push without verified full CI runs checks', async (t) => {
  const f = fixture(t);
  f.context.ref = 'refs/heads/main';
  f.commit({ 'README.md': 'updated\n' });
  f.job.steps[0].conclusion = 'skipped';
  assert.equal(await f.check(), true);
});

test('PR documentation skips heavy checks without GitHub API calls', async (t) => {
  const f = fixture(t);
  f.context.eventName = 'pull_request';
  f.context.ref = 'refs/pull/1/merge';
  f.context.payload.pull_request = { base: { sha: f.base } };
  f.commit({ 'docs/guide.md': 'guide', 'LICENSE': 'license' });
  assert.equal(await f.check(), false);
  assert.equal(f.calls.length, 0);
});

for (const file of ['main.go', 'go.mod', 'scripts/check.cjs', '.github/workflows/ci.yml', 'docs/migration.sql', '.agents/skills/repo-sync/SKILL.md', '.agents/skills/repo-sync/references/operations.md']) {
  test(`changes to ${file} run full checks`, async (t) => {
    const f = fixture(t);
    f.context.ref = 'refs/heads/main';
    f.commit({ [file]: 'changed\n' });
    assert.equal(await f.check(), true);
    assert.equal(f.calls.length, 0);
  });
}

test('removing an embedded skill reference runs full checks', async (t) => {
  const f = fixture(t);
  const reference = '.agents/skills/repo-sync/references/operations.md';
  const base = f.commit({ [reference]: 'embedded instructions\n' });
  f.context.eventName = 'pull_request';
  f.context.payload.pull_request = { base: { sha: base } };
  f.commit({ [reference]: null });
  assert.equal(await f.check(), true);
});

test('PR embedded skill changes cannot skip heavy checks', async (t) => {
  const f = fixture(t);
  f.context.eventName = 'pull_request';
  f.context.payload.pull_request = { base: { sha: f.base } };
  f.commit({ '.agents/skills/repo-sync/SKILL.md': 'embedded instructions\n' });
  assert.equal(await f.check(), true);
});

test('renaming code into documentation still runs checks', async (t) => {
  const f = fixture(t);
  f.context.ref = 'refs/heads/main';
  f.commit({ 'main.go': null, 'docs/code.md': 'package main\n' });
  assert.equal(await f.check(), true);
});

test('PR compares the entire branch diff, not only the latest docs commit', async (t) => {
  const f = fixture(t);
  f.context.eventName = 'pull_request';
  f.context.payload.pull_request = { base: { sha: f.base } };
  f.commit({ 'main.go': 'changed code\n' });
  f.commit({ 'README.md': 'updated\n' });
  assert.equal(await f.check(), true);
});

test('unavailable Git history runs checks', async (t) => {
  const f = fixture(t);
  f.context.ref = 'refs/heads/main';
  f.context.payload.before = 'a'.repeat(40);
  assert.equal(await f.check(), true);
});
