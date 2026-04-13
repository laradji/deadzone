:mod:`os` --- Miscellaneous operating system interfaces
=======================================================

.. module:: os
   :synopsis: Miscellaneous operating system interfaces.

**Source code:** :source:`Lib/os.py`

--------------

This module provides a portable way of using operating system dependent
functionality.  If you just want to read or write a file see :func:`open`,
if you want to manipulate paths, see the :mod:`os.path` module, and if
you want to read all the lines in all the files on the command line see
the :mod:`fileinput` module.

.. _os-procinfo:

Process Parameters
------------------

These functions and data items provide information and operate on the
current process and user.

.. function:: getcwd()

   Return a string representing the current working directory.

   Example::

      import os
      cwd = os.getcwd()
      print(cwd)

.. function:: chdir(path)

   Change the current working directory to *path*. See also
   :func:`getcwd`.

File Descriptor Operations
--------------------------

These functions operate on I/O streams referenced using file descriptors.

.. code-block:: python

   import os
   fd = os.open("foo.txt", os.O_RDONLY)
   data = os.read(fd, 1024)
   os.close(fd)

See :ref:`open() built-in function <open-builtin>` for the high-level
interface.
