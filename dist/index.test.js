'use strict';

const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const path = require('node:path');
const { spawnSync } = require('node:child_process');
const test = require('node:test');

const {
  childEnvironment,
  clearCredentialInputs,
} = require('./index.js');

test('childEnvironment passes only the controller allowlist', () => {
  const source = {
    PATH: '/tools',
    HOME: '/private/home',
    RUNNER_TEMP: '/private/temp',
    LocalAppData: 'C:\\Users\\runner\\AppData\\Local',
    HTTPS_PROXY: 'https://proxy.test',
    GITHUB_TOKEN: 'secret-token',
    'INPUT_GITHUB-TOKEN': 'secret-input',
    AWS_SECRET_ACCESS_KEY: 'secret-cloud',
    GIT_CONFIG_COUNT: '1',
  };
  const acquisition = childEnvironment(source, true);
  assert.equal(acquisition.PATH, '/tools');
  assert.equal(acquisition.LOCALAPPDATA, 'C:\\Users\\runner\\AppData\\Local');
  assert.equal(acquisition.LocalAppData, undefined);
  assert.equal(acquisition.HTTPS_PROXY, 'https://proxy.test');
  assert.equal(acquisition.GITHUB_TOKEN, undefined);
  assert.equal(acquisition['INPUT_GITHUB-TOKEN'], undefined);
  assert.equal(acquisition.AWS_SECRET_ACCESS_KEY, undefined);
  assert.equal(acquisition.GIT_CONFIG_COUNT, undefined);
  assert.equal(acquisition.GOENV, 'off');
  assert.equal(acquisition.GOWORK, 'off');
  assert.equal(acquisition.GOFLAGS, '');

  const execution = childEnvironment(source, false);
  assert.equal(execution.HTTPS_PROXY, undefined);
  assert.equal(execution.GOPROXY, 'off');
  assert.equal(execution.GOSUMDB, 'off');
  assert.equal(execution.GOTOOLCHAIN, 'local');
});

test('childEnvironment gives canonical names deterministic precedence', () => {
  const canonical = {
    PATH: '/trusted/tools',
    RUNNER_TEMP: '/trusted/temp',
  };
  const variants = {
    Path: '/attacker/tools',
    Runner_Temp: '/attacker/temp',
  };
  for (const source of [
    { ...canonical, ...variants },
    { ...variants, ...canonical },
  ]) {
    const environment = childEnvironment(source, true);
    assert.equal(environment.PATH, '/trusted/tools');
    assert.equal(environment.RUNNER_TEMP, '/trusted/temp');
    assert.equal(environment.Path, undefined);
    assert.equal(environment.Runner_Temp, undefined);
    assert.equal(Object.keys(environment).filter((name) => name.toUpperCase() === 'PATH').length, 1);
    assert.equal(Object.keys(environment).filter((name) => name.toUpperCase() === 'RUNNER_TEMP').length, 1);
  }

  const windowsSpelling = childEnvironment({
    Path: 'C:\\trusted\\bin',
    Runner_Temp: 'C:\\trusted\\temp',
  }, true);
  assert.equal(windowsSpelling.PATH, 'C:\\trusted\\bin');
  assert.equal(windowsSpelling.RUNNER_TEMP, 'C:\\trusted\\temp');

  const emptyCanonical = childEnvironment({
    Path: '/attacker/tools',
    PATH: '',
    Runner_Temp: '/attacker/temp',
    RUNNER_TEMP: '',
  }, true);
  assert.equal(emptyCanonical.PATH, '');
  assert.equal(emptyCanonical.RUNNER_TEMP, '');
});

test('childEnvironment rejects conflicting variants without a canonical name', () => {
  assert.throws(() => childEnvironment({
    Path: '/first/tools',
    pAtH: '/second/tools',
  }, true), /conflicting case variants for environment variable PATH/);
});

test('clearCredentialInputs overwrites and removes token inputs', () => {
  const environment = {
    GITHUB_TOKEN: 'token',
    'INPUT_GITHUB-TOKEN': 'input-token',
    INPUT_GITHUB_TOKEN: 'compat-token',
    PATH: '/tools',
  };
  clearCredentialInputs(environment);
  assert.deepEqual(environment, { PATH: '/tools' });
});

test('prepare-caller runs the caller command with plan metadata and no command-file credentials', {
  skip: process.platform === 'win32' ? 'planner hook runs on Ubuntu' : false,
}, () => {
  const result = spawnSync('bash', [path.join(__dirname, 'prepare-caller.sh')], {
    cwd: process.cwd(),
    encoding: 'utf8',
    env: {
      ...process.env,
      ACTIONS_RUNTIME_TOKEN: 'service-secret',
      GITHUB_ENV: '/tmp/github-env-command',
      GITHUB_OUTPUT: '/tmp/github-output-command',
      GITHUB_PATH: '/tmp/github-path-command',
      GITHUB_STATE: '/tmp/github-state-command',
      GITHUB_TOKEN: 'github-secret',
      SLIVER_EXT_E2E_ARCH: '',
      SLIVER_EXT_E2E_OS: '',
      SLIVER_EXT_E2E_PHASE: 'plan',
      SLIVER_EXT_E2E_PREPARE_COMMAND: 'env | sort',
      SLIVER_EXT_E2E_TARGET: '',
    },
  });
  assert.equal(result.status, 0, result.stderr);
  assert.match(result.stdout, /^SLIVER_EXT_E2E_PHASE=plan$/m);
  for (const name of [
    'ACTIONS_RUNTIME_TOKEN', 'GITHUB_ENV', 'GITHUB_OUTPUT', 'GITHUB_PATH',
    'GITHUB_STATE', 'GITHUB_TOKEN', 'SLIVER_EXT_E2E_PREPARE_COMMAND',
  ]) {
    assert.doesNotMatch(result.stdout, new RegExp(`^${name}=`, 'm'));
  }
});

test('manifest schema describes native libraries and extension arguments', () => {
  const schema = JSON.parse(fs.readFileSync(path.join(__dirname, '..', 'schema', 'sliver-ext-e2e-v1.schema.json'), 'utf8'));
  assert.equal(schema.properties.schema.const, 'sliver-ext-e2e/v1');
  const testShape = schema.$defs.test.allOf.find((shape) => shape.properties?.library);
  assert.ok(testShape.required.includes('library'));
  const libraryPattern = new RegExp(testShape.properties.library.pattern);
  for (const value of ['build/linux/amd64/smoke.so', 'build/windows/amd64/smoke.dll', 'build/darwin/arm64/smoke.dylib']) {
    assert.equal(libraryPattern.test(value), true, value);
  }
  for (const value of ['/tmp/smoke.so', 'C:/temp/smoke.dll', '../smoke.so', 'build\\smoke.so', 'smoke.o']) {
    assert.equal(libraryPattern.test(value), false, value);
  }
  assert.ok(schema.$defs.settings.properties.export, 'extension export is missing');
  assert.ok(schema.$defs.settings.properties.init, 'optional init export is missing');
  assert.ok(schema.$defs.settings.properties.args_utf8, 'UTF-8 arguments are missing');
  assert.ok(schema.$defs.settings.properties.args_base64, 'binary arguments are missing');
  assert.deepEqual(schema.$defs.settings.not.required, ['args_utf8', 'args_base64']);
  assert.equal(schema.$defs.target.oneOf.length, 3);
});

test('prepared runtime replaces its environment before executing extensions', {
  skip: process.platform === 'win32' ? 'POSIX shell fixture' : false,
}, () => {
  const directory = fs.mkdtempSync(path.join(os.tmpdir(), 'sliver-ext-runtime-test-'));
  try {
    const controller = path.join(directory, 'controller');
    const descriptor = path.join(directory, 'prepared.json');
    const state = path.join(directory, 'state.json');
    fs.writeFileSync(controller, `#!/bin/sh
env | sort > "${controller}.env"
printf '%s\\n' "$@" > "${controller}.args"
`, { mode: 0o700 });
    fs.writeFileSync(descriptor, '{}\n', { mode: 0o600 });

    const result = spawnSync('bash', [path.join(__dirname, 'run-prepared.sh')], {
      cwd: directory,
      encoding: 'utf8',
      env: {
        ...process.env,
        ACTIONS_RUNTIME_TOKEN: 'automatic-service-secret',
        AWS_SECRET_ACCESS_KEY: 'unrelated-job-secret',
        DISPLAY: ':99',
        XAUTHORITY: path.join(directory, 'xauthority'),
        GITHUB_TOKEN: 'forwarded-token',
        GITHUB_OUTPUT: path.join(directory, 'workflow-output'),
        'INPUT_GITHUB-TOKEN': 'action-input-secret',
        RUNNER_TEMP: directory,
        GITHUB_WORKSPACE: directory,
        SLIVER_EXT_ACTION_VERSION: 'owner/action@commit',
        SLIVER_EXT_CONTROLLER: controller,
        SLIVER_EXT_MANIFEST: 'manifest.yml',
        SLIVER_EXT_PREPARED_FILE: descriptor,
        SLIVER_EXT_RESULTS_DIR: path.join(directory, 'results'),
        SLIVER_EXT_SLIVER_VERSION: 'master',
        SLIVER_EXT_STATE_FILE: state,
        SLIVER_EXT_TARGET: 'darwin/arm64',
      },
    });
    assert.equal(result.status, 0, result.stderr);

    const runtimeEnvironment = fs.readFileSync(`${controller}.env`, 'utf8');
    for (const name of [
      'ACTIONS_RUNTIME_TOKEN', 'AWS_SECRET_ACCESS_KEY', 'GITHUB_OUTPUT',
      'GITHUB_TOKEN', 'INPUT_GITHUB-TOKEN',
    ]) {
      assert.doesNotMatch(runtimeEnvironment, new RegExp(`^${name}=`, 'm'));
    }
    assert.match(runtimeEnvironment, new RegExp(`^RUNNER_TEMP=${directory}$`, 'm'));
    assert.match(runtimeEnvironment, /^DISPLAY=:99$/m);
    assert.match(runtimeEnvironment, new RegExp(`^XAUTHORITY=${directory}/xauthority$`, 'm'));

    const arguments_ = fs.readFileSync(`${controller}.args`, 'utf8');
    assert.match(arguments_, /--prepared\n/);
    assert.match(arguments_, /darwin\/arm64\n/);
    assert.doesNotMatch(arguments_, /--github-output/);
  } finally {
    fs.rmSync(directory, { recursive: true, force: true });
  }
});
