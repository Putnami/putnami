namespace Putnami.Example.Library;

/// <summary>The price a percentage discount leaves.</summary>
public static class Discount
{
    /// <summary>Applies a discount of <paramref name="percent"/> to <paramref name="price"/>, in cents.</summary>
    public static int Apply(int price, int percent)
    {
        if (percent <= 0)
        {
            return price;
        }

        if (percent >= 100)
        {
            return 0;
        }

        return price - (price * percent / 100);
    }
}
