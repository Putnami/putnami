<?php

declare(strict_types=1);

namespace App\Tests;

use App\Calc;
use PHPUnit\Framework\Attributes\DataProvider;
use PHPUnit\Framework\TestCase;

final class CalcTest extends TestCase
{
    public static function sums(): array
    {
        return ['small' => [2, 3, 5], 'zero' => [0, 0, 0]];
    }

    #[DataProvider('sums')]
    public function testAddSumsTwoNumbers(int $a, int $b, int $expected): void
    {
        $this->assertSame($expected, (new Calc())->add($a, $b));
    }

    public function testSignReturnsPositive(): void
    {
        $this->assertSame('positive', (new Calc())->sign(4));
    }

    public function testAddIsWrongOnPurpose(): void
    {
        $this->assertSame(5, (new Calc())->add(2, 2), 'two plus two should be five');
    }

    public function testSkipped(): void
    {
        $this->markTestSkipped('not ready');
    }
}
