import pytest

from pylib import clamp, fibonacci, is_prime


@pytest.mark.putnami_proves("python/example-library-utilities", "clamp-bounds", "clamp-returns-a-value-already-inside-the-range")
def test_clamp_within_range():
    assert clamp(5, 0, 10) == 5


@pytest.mark.putnami_proves("python/example-library-utilities", "clamp-bounds", "clamp-clamps-out-of-range-values")
def test_clamp_below_min():
    assert clamp(-5, 0, 10) == 0


@pytest.mark.putnami_proves("python/example-library-utilities", "clamp-bounds", "clamp-clamps-out-of-range-values")
def test_clamp_above_max():
    assert clamp(15, 0, 10) == 10


def test_clamp_at_min():
    assert clamp(0, 0, 10) == 0


def test_clamp_at_max():
    assert clamp(10, 0, 10) == 10


def test_fibonacci_zero():
    assert fibonacci(0) == []


def test_fibonacci_one():
    assert fibonacci(1) == [0]


def test_fibonacci_five():
    assert fibonacci(5) == [0, 1, 1, 2, 3]


def test_fibonacci_ten():
    assert fibonacci(10) == [0, 1, 1, 2, 3, 5, 8, 13, 21, 34]


def test_is_prime_small():
    assert is_prime(0) is False
    assert is_prime(1) is False
    assert is_prime(2) is True
    assert is_prime(3) is True
    assert is_prime(4) is False


def test_is_prime_larger():
    assert is_prime(7) is True
    assert is_prime(11) is True
    assert is_prime(13) is True
    assert is_prime(15) is False
    assert is_prime(17) is True
    assert is_prime(97) is True
    assert is_prime(100) is False
