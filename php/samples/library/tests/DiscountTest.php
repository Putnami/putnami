<?php

declare(strict_types=1);

namespace PutnamiExample\Library\Tests;

use InvalidArgumentException;
use PHPUnit\Framework\TestCase;
use PutnamiExample\Library\Discount;

final class DiscountTest extends TestCase
{
    public function testItReducesAnAmount(): void
    {
        self::assertSame(900, (new Discount(10))->applyTo(1000));
    }

    public function testItRefusesAPercentAboveOneHundred(): void
    {
        $this->expectException(InvalidArgumentException::class);

        new Discount(101);
    }
}
