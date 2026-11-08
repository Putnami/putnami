<?php

declare(strict_types=1);

namespace PutnamiExample\Application;

use PutnamiExample\Library\Discount;

final class Checkout
{
    public function __construct(private readonly Discount $discount)
    {
    }

    /**
     * @param list<int> $cents
     */
    public function total(array $cents): int
    {
        return $this->discount->applyTo(array_sum($cents));
    }
}
