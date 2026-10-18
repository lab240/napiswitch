# Built on the board by deploy/install.sh (needs linux-headers for the running kernel).
obj-m := kszreg.o

all:
	$(MAKE) -C /lib/modules/$(shell uname -r)/build M=$(CURDIR) modules

clean:
	$(MAKE) -C /lib/modules/$(shell uname -r)/build M=$(CURDIR) clean
