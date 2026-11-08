<?php

declare(strict_types=1);

namespace PutnamiExample\Library;

use InvalidArgumentException;

final class Discount
{
    public function __construct(private readonly int $percent)
    {
        if ($percent < 0 || $percent > 100) {
            throw new InvalidArgumentException('A discount is between 0 and 100 percent.');
        }
    }

    public function applyTo(int $cents): int
    {
        return (int) round($cents * (100 - $this->percent) / 100);
    }
}
