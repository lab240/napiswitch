// One-shot access to a KSZ switch register through the ksz driver's own
// regmap (spi2.0, 8-bit map). Loads, does the job, logs, refuses to stay.
//   insmod kszreg.ko reg=0x6301            - read
//   insmod kszreg.ko reg=0x6301 mask=0x04 val=0x04   - update bits
#include <linux/module.h>
#include <linux/spi/spi.h>
#include <linux/regmap.h>

static char *dev_name_p = "spi2.0";
module_param_named(dev, dev_name_p, charp, 0);
static uint reg;
module_param(reg, uint, 0);
static uint mask;
module_param(mask, uint, 0);
static uint val;
module_param(val, uint, 0);

static int __init kszreg_init(void)
{
	struct device *d;
	struct regmap *map;
	unsigned int before = 0, after = 0;
	int err;

	d = bus_find_device_by_name(&spi_bus_type, NULL, dev_name_p);
	if (!d)
		return -ENODEV;
	map = dev_get_regmap(d, "8");
	if (!map) {
		put_device(d);
		return -ENXIO;
	}
	err = regmap_read(map, reg, &before);
	if (!err && mask)
		err = regmap_update_bits(map, reg, mask, val & mask);
	if (!err)
		err = regmap_read(map, reg, &after);
	pr_info("kszreg: %s reg 0x%04x: 0x%02x -> 0x%02x (mask 0x%02x) err %d\n",
		dev_name_p, reg, before, after, mask, err);
	put_device(d);
	return err ? err : -EAGAIN; /* never stay loaded */
}

module_init(kszreg_init);
MODULE_LICENSE("GPL");
MODULE_DESCRIPTION("one-shot KSZ register access for debugging");
