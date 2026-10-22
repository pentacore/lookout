<?php
/* Enrolls this server from the Lookout settings page and answers with JSON
 * {"ok": bool, "output": string}. The webGUI's local_prepend.php has already
 * rejected any POST without a valid csrf_token. Both inputs are validated, the
 * enroll script runs without a shell, and the token is never echoed back.
 * (Unraid's update.php is not used: it writes the command line, token
 * included, to syslog.) */
if (!defined('LOOKOUT_ENROLL_SCRIPT')) {
  define('LOOKOUT_ENROLL_SCRIPT', '/usr/local/emhttp/plugins/lookout/scripts/enroll');
}

$lookout_reply = function ($ok, $output, $code) {
  http_response_code($code);
  header('Content-Type: application/json');
  header('Cache-Control: no-store');
  header('X-Content-Type-Options: nosniff');
  echo json_encode(['ok' => $ok, 'output' => $output], JSON_HEX_TAG | JSON_HEX_AMP | JSON_HEX_APOS | JSON_HEX_QUOT | JSON_INVALID_UTF8_SUBSTITUTE), "\n";
  exit;
};

if (($_SERVER['REQUEST_METHOD'] ?? '') !== 'POST') {
  $lookout_reply(false, 'use POST', 405);
}

/* https:// for any host; plain http:// only for loopback hosts, like the agent. */
$lookout_url = $_POST['url'] ?? '';
if (!is_string($lookout_url) || (
  preg_match('~\Ahttps://[A-Za-z0-9.-]+(:[0-9]+)?(/[[:graph:]]*)?\z~', $lookout_url) !== 1 &&
  preg_match('~\Ahttp://(localhost|127\.0\.0\.1|\[::1\])(:[0-9]+)?(/[[:graph:]]*)?\z~', $lookout_url) !== 1
)) {
  $lookout_reply(false, 'invalid platform URL', 400);
}
$lookout_token = $_POST['token'] ?? '';
if (!is_string($lookout_token) || preg_match('/\A[A-Za-z0-9]{32}\z/', $lookout_token) !== 1) {
  $lookout_reply(false, 'invalid enrollment token', 400);
}

$lookout_proc = proc_open(
  [LOOKOUT_ENROLL_SCRIPT, $lookout_url, $lookout_token],
  [0 => ['file', '/dev/null', 'r'], 1 => ['pipe', 'w'], 2 => ['redirect', 1]],
  $lookout_pipes
);
if (!is_resource($lookout_proc)) {
  $lookout_reply(false, 'cannot run the enroll script', 500);
}
$lookout_output = (string) stream_get_contents($lookout_pipes[1]);
fclose($lookout_pipes[1]);
$lookout_status = proc_close($lookout_proc);
$lookout_reply($lookout_status === 0, trim(str_replace($lookout_token, '[redacted]', $lookout_output)), 200);
