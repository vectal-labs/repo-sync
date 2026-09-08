const { execFileSync } = require('node:child_process');

// True means run every check. Missing history or API errors never skip checks.
module.exports = async function shouldRunChecks({ github, context, core,
  git = (args) => execFileSync('git', args, { encoding: 'utf8' }),
}) {
  const docsOnly = (range) => git(['diff', '--no-renames', '--name-only', '-z', ...range, '--'])
    .split('\0').filter(Boolean)
    .every((file) => !file.startsWith('.agents/skills/repo-sync/') &&
      (file.endsWith('.md') || file === 'LICENSE' ||
        /^docs\/.+\.(png|jpe?g|gif|svg|webp|ico|pdf)$/i.test(file)));

  try {
    const head = git(['rev-parse', 'HEAD']).trim();
    const tag = context.eventName === 'push' && context.ref.startsWith('refs/tags/v');
    if (!tag) {
      if (context.eventName === 'pull_request') {
        const base = context.payload.pull_request.base.sha;
        const required = !docsOnly([`${base}...${head}`]);
        core.info(required ? 'Code changed; running all checks.' : 'Documentation-only PR; skipping heavy checks.');
        return required;
      }
      const before = context.payload.before;
      if (context.eventName !== 'push' || context.ref !== 'refs/heads/main' ||
          !before || /^0+$/.test(before) || !docsOnly([before, head])) return true;
    }

    const { data } = await github.rest.actions.listWorkflowRuns({
      ...context.repo, workflow_id: 'ci.yml', event: 'push', branch: 'main',
      status: 'success', per_page: 20, ...(tag ? { head_sha: head } : {}),
    });
    for (const run of data.workflow_runs) {
      if (run.event !== 'push' || run.head_branch !== 'main' || run.conclusion !== 'success' ||
          run.head_repository?.full_name !== `${context.repo.owner}/${context.repo.repo}` ||
          (tag && run.head_sha !== head)) continue;
      const { data: { jobs } } = await github.rest.actions.listJobsForWorkflowRun({
        ...context.repo, run_id: run.id, filter: 'latest', per_page: 100,
      });
      const tested = jobs.some((job) => job.name === 'test' && job.head_sha === run.head_sha &&
        job.conclusion === 'success' && job.steps.some((step) =>
          step.name === 'Run go test -race -count=1 ./...' && step.conclusion === 'success'));
      if (!tested) continue;
      // Compare against fully tested code, so cancelling a preceding code push
      // cannot let a later docs-only push hide those untested changes.
      if (tag || docsOnly([run.head_sha, head])) {
        core.info(tag ? `Reusing full main CI: ${run.html_url}` : `Only documentation changed since full CI: ${run.html_url}`);
        return false;
      }
      break;
    }
    core.info('No reusable full main CI; running all checks.');
  } catch (error) {
    core.warning(`Cannot establish reusable checks; running all checks: ${error.message}`);
  }
  return true;
};
