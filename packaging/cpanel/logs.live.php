<?php
require_once __DIR__ . '/proxy.php';
$cpanel = gniza_cpanel();
if (!gniza_feature_enabled()) {
    http_response_code(403);
    print $cpanel->header('Gniza');
    print '<p>Gniza is not enabled for this account.</p>';
    print $cpanel->footer();
    exit;
}
ob_start();
print $cpanel->header('Gniza');
gniza_page('/logs');
print $cpanel->footer();
ob_end_flush();
