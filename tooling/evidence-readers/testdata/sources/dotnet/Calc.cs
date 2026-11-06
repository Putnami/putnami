namespace Calc;

public static class Calculator
{
    public static int Add(int a, int b) => a + b;

    public static string Sign(int n)
    {
        if (n >= 0)
        {
            return "positive";
        }
        return "negative";
    }

    public static int NeverCalled(int x)
    {
        var doubled = x * 2;
        return doubled + 1;
    }
}
