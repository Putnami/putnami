<?php

declare(strict_types=1);

namespace App;

final class Calc
{
    public function add(int $a, int $b): int
    {
        return $a + $b;
    }

    public function sign(int $n): string
    {
        if ($n >= 0) {
            return 'positive';
        } else {
            return 'negative';
        }
    }

    public function neverCalled(int $x): int
    {
        $doubled = $x * 2;
        return $doubled + 1;
    }
}
