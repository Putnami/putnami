<?php

declare(strict_types=1);

require dirname(__DIR__) . '/vendor/autoload.php';

use PutnamiExample\Application\Checkout;
use PutnamiExample\Library\Discount;

header('Content-Type: text/plain');
echo (new Checkout(new Discount(10)))->total([1000, 500]), "\n";
