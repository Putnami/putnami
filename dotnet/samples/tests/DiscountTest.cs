using Putnami.Example.Library;

namespace Putnami.Example.Tests;

public class DiscountTest
{
    [Fact]
    public void AppliesThePercentage() => Assert.Equal(8500, Discount.Apply(10000, 15));

    [Fact]
    public void KeepsThePriceWithNoDiscount() => Assert.Equal(10000, Discount.Apply(10000, 0));

    [Fact]
    public void NeverGoesBelowZero() => Assert.Equal(0, Discount.Apply(10000, 120));
}
