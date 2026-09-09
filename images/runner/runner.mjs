#!/usr/bin/env node
/**
 * Cloud-sandbox task runner (add-cloud-executor 4.3, design D4/D8).
 *
 * This is what actually runs inside a sandbox container. The only credential
 * it will ever hold is the per-task token (TASK_TOKEN): it is the API key for
 * the platform LLM outlet (ANTHROPIC_BASE_URL points back at the public API
 * -- the egress lockdown blocks the intranet upstream by design) and the
 * Bearer token for the terminal callback. No control-plane secret exists in
 * this image, and the token must never be echoed into artifacts.
 *
 * Contract with the API (routes/sandbox-callback.ts):
 *   POST /api/sandbox/task-result { artifacts, summary, attempt? }
 *   POST /api/sandbox/task-error   { error, errorType, attempt? }
 * `attempt` is forwarded when the container was given ATTEMPT (G19 attempt
 * pairing: the callback must name the dispatch generation that ran it).
 */
import { spawnSync } from 'node:child_process'

const env = process.env

function die(msg) {
  console.error(`runner: ${msg}`)
  process.exit(78) // EX_CONFIG: the box was started wrong, nothing to retry
}

for (const name of ['TASK_TOKEN', 'API_BASE_URL', 'MISSION_ID', 'TASK_ID', 'TASK_PROMPT']) {
  if (!env[name]) die(`${name} is not set -- the sandbox_up item was incomplete`)
}

const base = env.API_BASE_URL.replace(/\/+$/, '')
const attempt = env.ATTEMPT !== undefined && env.ATTEMPT !== ''
  ? { attempt: Number(env.ATTEMPT) }
  : {}

/** Terminal callbacks. Best effort: if the callback itself fails, the API's
 * timeout sweep destroys the box and settles the task, so we log and exit. */
async function post(path, body) {
  try {
    const res = await fetch(base + path, {
      method: 'POST',
      headers: { Authorization: `Bearer ${env.TASK_TOKEN}`, 'Content-Type': 'application/json' },
      body: JSON.stringify({ ...body, ...attempt }),
    })
    if (!res.ok) console.error(`runner: callback ${path} -> ${res.status}`)
  } catch (err) {
    console.error(`runner: callback ${path} failed: ${err && err.message}`)
  }
}

/** Claude CLI reports the commit it made as `commit_hash` when it used git. */
function extractCommitHash(text) {
  const m = /(?:commit[ :]+)([0-9a-f]{7,40})/i.exec(text || '')
  return m ? m[1] : undefined
}

const summaryOf = (text) => (text || '').trim().slice(0, 400) || '(no output)'

async function main() {
  const args = ['-p', '--output-format', 'json', '--dangerously-skip-permissions']
  if (env.MODEL) args.push('--model', env.MODEL)

  const cli = spawnSync('claude', args, {
    cwd: '/workspace',
    encoding: 'utf8',
    timeout: 25 * 60 * 1000, // stop before the API's timeout sweep destroys us
    env: {
      ...env,
      // The platform outlet speaks the Anthropic Messages API; the task token
      // IS the API key here (D4: the credential is the billing anchor).
      ANTHROPIC_BASE_URL: `${base}/api/sandbox/llm`,
      ANTHROPIC_AUTH_TOKEN: env.TASK_TOKEN,
      DISABLE_TELEMETRY: '1',
      DISABLE_ERROR_REPORTING: '1',
    },
  })

  if (cli.error) {
    await post('/api/sandbox/task-error', {
      error: `runner CLI failed to start: ${cli.error.message}`,
      errorType: 'runner_spawn_error',
    })
    return
  }

  // -p --output-format json wraps the session in one JSON object; the text we
  // report as artifacts is `result`, the CLI's final answer.
  let resultText = ''
  let cliError = ''
  try {
    const parsed = JSON.parse(cli.stdout)
    resultText = typeof parsed.result === 'string' ? parsed.result : cli.stdout
    cliError = parsed.is_error ? (parsed.result || 'CLI reported an error') : ''
  } catch {
    resultText = cli.stdout // non-JSON (interrupted stream): report it verbatim
  }

  if (cli.status === 0 && !cliError) {
    await post('/api/sandbox/task-result', {
      artifacts: resultText,
      summary: summaryOf(resultText),
      commitHash: extractCommitHash(resultText),
    })
  } else {
    await post('/api/sandbox/task-error', {
      error: (cliError || cli.stderr || `CLI exited ${cli.status}`).slice(0, 2000),
      errorType: cli.status === null ? 'runner_timeout' : 'cli_error',
    })
  }
}

main().catch(async (err) => {
  await post('/api/sandbox/task-error', {
    error: `runner crashed: ${err && err.stack || err}`,
    errorType: 'runner_crash',
  })
})
