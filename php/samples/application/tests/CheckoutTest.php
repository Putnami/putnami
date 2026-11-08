<?php

declare(strict_types=1);

namespace PutnamiExample\Application\Tests;

use PHPUnit\Framework\TestCase;
use PutnamiExample\Application\Checkout;
use PutnamiExample\Library\Discount;

final class CheckoutTest extends TestCase
{
    public function testItDiscountsTheSumOfTheLines(): void
    {
        self::assertSame(1350, (new Checkout(new Discount(10)))->total([1000, 500]));
    }
}
